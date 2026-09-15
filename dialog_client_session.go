// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
)

var (
	ErrClientEarlyMedia = errors.New("Early media detected")
)

// DialogClientSession represents outbound channel
type DialogClientSession struct {
	*sipgo.DialogClientSession

	dialogMedia

	onReferDialog OnReferDialogFunc
	mediaConfig   MediaConfig

	closed atomic.Uint32
}

// DialogMedia returns the media half of the session. It is never nil.
func (d *DialogClientSession) DialogMedia() *DialogMedia {
	return &d.dialogMedia
}

// Close frees local resources (media stack and dialog cleanup hooks).
// It is idempotent and does NOT send any SIP message: closing without Hangup
// leaves the remote leg up. Signaling teardown is the caller's responsibility
// on client dialogs (docs/contracts.md §6, §7).
func (d *DialogClientSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.dialogMedia.Close()
	e2 := d.DialogClientSession.Close()
	return errors.Join(e1, e2)
}

func (d *DialogClientSession) ID() string {
	return d.DialogClientSession.ID
}

// Hangup terminates dialog with BYE.
// Options allow customizing headers of the BYE request (ex. Reason header),
// Contact and to mutate the final request.
//
// Behavior by dialog state (docs/contracts.md §7):
//   - no response received yet: error ErrDialogNotAnswered (cancel the Invite
//     context instead, which sends CANCEL)
//   - provisional response received (early dialog): BYE on the early dialog
//   - confirmed: BYE, waits for 200
//   - already ended: returns nil silently
//
// Scope: WithHeaders, WithContact, WithRequestMutator.
func (d *DialogClientSession) Hangup(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Hangup", scopeClientHangup)
	if err != nil {
		return err
	}
	return d.byeSignal(ctx, params)
}

// HangupAndClose terminates the dialog with Hangup (sending BYE when
// possible) and then releases local resources with Close. The local close
// always runs, so resources are not leaked even when Hangup fails.
func (d *DialogClientSession) HangupAndClose(ctx context.Context, opts ...SignalOption) error {
	return errors.Join(d.Hangup(ctx, opts...), d.Close())
}

// byeSignal builds BYE request applying signaling options and writes it.
// Mandatory dialog headers (From/To/Call-ID/CSeq) are filled by sipgo.
func (d *DialogClientSession) byeSignal(ctx context.Context, params *signalParams) error {
	if d.InviteResponse == nil {
		return byeNotPossible()
	}
	inviteRequest := d.InviteRequest
	recipient := inviteRequest.Recipient
	if contact := d.InviteResponse.Contact(); contact != nil {
		recipient = contact.Address
	}

	bye := sip.NewRequest(sip.BYE, recipient)
	bye.SipVersion = inviteRequest.SipVersion
	if len(inviteRequest.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", inviteRequest, bye)
	}
	bye.SetTransport(inviteRequest.Transport())
	bye.SetSource(inviteRequest.Source())

	if err := applyRequestSignal(bye, params); err != nil {
		return err
	}
	return d.DialogClientSession.WriteBye(ctx, bye)
}

// byeNotPossible is returned when no BYE can be sent because the dialog was
// never established (no invite response received yet).
func byeNotPossible() error {
	return fmt.Errorf("bye: %w", ErrDialogNotAnswered)
}

func (d *DialogClientSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

func (d *DialogClientSession) ToUser() string {
	return d.InviteRequest.To().Address.User
}

// Transport returns the transport of the dialog's INVITE request (ex. "UDP").
func (d *DialogClientSession) Transport() string {
	return d.InviteRequest.Transport()
}

func (d *DialogClientSession) DialogSIP() *sipgo.Dialog {
	return &d.Dialog
}

func (d *DialogClientSession) RemoteContact() *sip.ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remoteContactUnsafe()
}

func (d *DialogClientSession) remoteContactUnsafe() *sip.ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	return d.InviteResponse.Contact()
}

// Invite sends Invite request and establishes [early] media. Normally you need to call Ack after.
//
// Normal Answer with 200 OK (SDP)
// - You MUST call Ack() after to acknowledge session.
//
// Early Media Detect:
// - WithEarlyMediaDetect() must be set as part of options otherwise it ignores early media
// - It RETURNS ErrClientEarlyMedia if remote answers with 183 Session in Progress
// - Media is negotiated and setuped
// - You need to call WaitAnswer() if you want to proceed with answering call
//
// Options allow customizing Contact, From, custom headers, SDP body, media
// (IP, codecs, fully custom media session) and a final request mutator.
//
// Scope: WithHeaders, WithContact, WithBody, WithRequestMutator, WithOriginator,
// WithAuthCredentials, WithEarlyMediaDetect, WithOnResponse, WithOnMediaUpdate,
// WithOnRefer, plus the media overrides (WithCodecs, WithRTPNAT, WithMediaBindIP,
// WithMediaExternalIP, WithMediaDTLS, WithSDPSessionName, WithMediaSession,
// WithMusicOnHold).
//
// Errors:
// - sipgo.ErrDialogResponse
// - ErrClientEarlyMedia
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
// If you pass originator it will use originator to set correct from header and avoid media transcoding
func (d *DialogClientSession) Invite(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Invite", scopeClientInvite)
	if err != nil {
		return err
	}
	return d.inviteWithParams(ctx, params)
}

// inviteWithParams performs the Invite flow using a pre-computed *signalParams.
// It exists so callers (e.g. Diago.Dial, DialBridge) can compute params once
// and avoid running user-supplied SignalOption funcs twice.
func (d *DialogClientSession) inviteWithParams(ctx context.Context, params *signalParams) error {
	if params == nil || params.Media.MediaSession == nil {
		conf := signalMediaConfig(d.mediaConfig, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return err
		}
	} else if d.mediaSession == nil {
		// Use custom pre-created media session. When combined with an
		// Originator we must Fork the session: the originator branch in
		// invite() rewrites Codecs and applies RemoteSDP/SetRemoteAddr, and
		// we don't want to mutate a session that the caller may reuse.
		sess := params.Media.MediaSession
		if params.Dialog.Originator != nil {
			sess = params.Media.MediaSession.Fork()
		}
		d.mu.Lock()
		if d.mediaSession == nil {
			d.initMediaSessionUnsafe(sess, nil, nil)
		}
		d.mu.Unlock()
	}
	return d.invite(ctx, &d.dialogMedia, params)
}

func (d *DialogClientSession) invite(ctx context.Context, med *DialogMedia, params *signalParams) error {
	sess := med.mediaSession
	inviteReq := d.InviteRequest
	originator := params.Dialog.Originator

	// Custom headers must be applied before originator logic so it can
	// detect user provided From header
	applySignalHeaders(inviteReq, params.Msg.Headers)

	if originator != nil {
		// In case originator then:
		// - check do we support this media formats by conf
		// - if we do, then filter and pass to dial endpoint filtered
		origInvite := originator.DialogSIP().InviteRequest
		if fromHDR := inviteReq.From(); fromHDR == nil {
			// From header should be preserved from originator
			fromHDROrig := origInvite.From()
			f := sip.FromHeader{
				DisplayName: fromHDROrig.DisplayName,
				Address:     *fromHDROrig.Address.Clone(),
				Params:      fromHDROrig.Params.Clone(),
			}
			inviteReq.AppendHeader(&f)
		}

		// Avoid transcoding if originator present
		// Check ContentType and body present
		contType := origInvite.ContentType()
		if body := origInvite.Body(); body != nil && (contType != nil && contType.Value() == "application/sdp") {
			// apply remote SDP
			if err := sess.RemoteSDP(body); err != nil {
				return fmt.Errorf("failed to apply originator sdp: %w", err)
			}
			// We do not want originator to be remote side, but we want to apply codec filtering
			sess.SetRemoteAddr(&net.UDPAddr{})

			// Now to totally remove transcoding a chance. Leave only one codec of different types
			audioCodec := media.Codec{}
			telEventCodec := media.Codec{}

			codecs := sess.CommonCodecs()
			if len(codecs) == 0 { // No negotiation yet happened
				codecs = sess.Codecs
			}

			for _, c := range codecs {
				// TODO refactor this
				if strings.HasPrefix(c.Name, "telephone-event") {
					if telEventCodec.SampleRate == 0 {
						telEventCodec = c
					}
					continue
				}

				if audioCodec.SampleRate == 0 {
					audioCodec = c
				}
			}

			// TODO: DO we need to be thread safe here?
			// In this case we want to rewrite what should be Offered in our SDP
			// NOTE: Generally this would require Session Fork, but for now we avoid this extra step.
			sessCodecs := sess.Codecs[:0]
			if audioCodec.SampleRate != 0 {
				sessCodecs = append(sessCodecs, audioCodec)
			}

			// TODO: should we only match telephone event with same sampling rate?
			if telEventCodec.SampleRate != 0 {
				sessCodecs = append(sessCodecs, telEventCodec)
			}

			if len(sessCodecs) == 0 {
				return fmt.Errorf("no codecs support found from originator")
			}
			sess.Codecs = sessCodecs
		}
	}

	dialogCli := d.UA
	// Honor custom Contact, otherwise use dialog default one
	if params.Msg.Contact != nil {
		setSignalContact(inviteReq, params.Msg.Contact)
	} else {
		inviteReq.AppendHeader(&dialogCli.ContactHDR)
	}

	body := sess.LocalSDP()
	if params.Msg.Body != nil {
		body = params.Msg.Body
	}
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody(body)

	// We allow changing full from header, but we need to make sure it is correctly set
	// If users specify 'tag' parameter it is assumed that they know what they do
	if fromHDR := inviteReq.From(); fromHDR != nil && !fromHDR.Params.Has("tag") {
		fromHDR.Params.Add("tag", sip.GenerateTagN(16))
	}

	// Last chance for full customization of the request
	if params.Msg.MutateRequest != nil {
		if err := params.Msg.MutateRequest(inviteReq); err != nil {
			return err
		}
	}

	// Build here request
	client := d.UA.Client
	if err := sipgo.ClientRequestBuild(client, inviteReq); err != nil {
		return err
	}

	// This only gets called after session established.
	// These are read under d.mu in handleRefer and handleReInviteACK,
	// so write them under the same lock to avoid a data race.
	d.mu.Lock()
	med.onMediaUpdate = params.Dialog.OnMediaUpdate
	d.onReferDialog = params.Dialog.OnRefer
	d.mu.Unlock()
	// reuse UDP listener
	// Problem if listener is unspecified IP sipgo will not map this to listener
	// Code below only works if our bind host is specified
	// For now let SIPgo create 1 UDP connection and it will reuse it
	// via := inviteReq.Via()
	// if via.Host == "" {
	// }
	err := d.DialogClientSession.Invite(ctx, func(c *sipgo.Client, req *sip.Request) error {
		// Do nothing
		return nil
	})
	if err != nil {
		// sess.Close()
		return err
	}
	ansOpts := answerOptionsFromParams(params)

	if params.Dialog.EarlyMediaDetect {
		return d.waitAnswerEarly(ctx, med, ansOpts)
	}

	return d.waitAnswer(ctx, med, ansOpts)
}

// answerOptionsFromParams extracts the sipgo-level answer options from parsed
// signalParams. It is the bridge between diago's SignalOption surface and the
// sipgo DialogClientSession API.
func answerOptionsFromParams(params *signalParams) sipgo.AnswerOptions {
	if params == nil {
		return sipgo.AnswerOptions{}
	}
	return sipgo.AnswerOptions{
		Username:   params.Dialog.Username,
		Password:   params.Dialog.Password,
		OnResponse: params.Dialog.OnResponse,
	}
}

// WaitAnswer waits for the final answer of a dialog whose Invite returned
// ErrClientEarlyMedia (or otherwise did not complete). Media options are
// ignored here: the media session was already negotiated by Invite.
// Options allow receiving responses (WithOnResponse) and digest credentials
// (WithAuthCredentials).
// Scope: WithOnResponse, WithAuthCredentials.
func (d *DialogClientSession) WaitAnswer(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "WaitAnswer", scopeWaitAnswer)
	if err != nil {
		return err
	}
	return d.waitAnswer(ctx, &d.dialogMedia, answerOptionsFromParams(params))
}

func (d *DialogClientSession) waitAnswerEarly(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	sess := med.mediaSession
	onResps := opts.OnResponse

	// Add early media check
	opts.OnResponse = func(res *sip.Response) error {
		// https://datatracker.ietf.org/doc/html/rfc3261#section-8.1.3.2
		// 		UAC MUST treat any provisional response different than 100 that it
		//    does not recognize as 183 (Session Progress).
		// Check any existing
		if onResps != nil {
			if err := onResps(res); err != nil {
				return err
			}
		}

		// handle 183 Session Progress early media
		if res.StatusCode != sip.StatusSessionInProgress {
			return nil
		}

		if cont := res.ContentType(); cont == nil || cont.Value() != "application/sdp" {
			return nil
		}

		remoteSDP := res.Body()
		if remoteSDP == nil {
			return nil
		}

		if err := sess.RemoteSDP(remoteSDP); err != nil {
			return err
		}

		if err := sess.Finalize(); err != nil {
			return err
		}

		rtpSess := media.NewRTPSession(sess)
		med.mu.Lock()
		med.initRTPSessionUnsafe(sess, rtpSess)
		med.onCloseUnsafe(func() error {
			return rtpSess.Close()
		})
		med.mu.Unlock()

		// Must be called after reader and writer setup due to race
		if err := rtpSess.MonitorBackground(); err != nil {
			return err
		}

		return ErrClientEarlyMedia
	}
	return d.waitAnswer(ctx, med, opts)
}

func (d *DialogClientSession) waitAnswer(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	if err := d.DialogClientSession.WaitAnswer(ctx, opts); err != nil {
		return err
	}

	remoteSDP := d.InviteResponse.Body()
	if remoteSDP == nil {
		return fmt.Errorf("no SDP in response: %w", ErrSDPMissing)
	}

	if err := d.applyRemoteSDP(med, remoteSDP); err != nil {
		// Terminate call. Call must be ACK before doing BYE
		ackErr := d.Ack(ctx)
		byeErr := d.Bye(ctx)
		return errors.Join(err, ackErr, byeErr)
	}

	return nil
}

func (d *DialogClientSession) applyRemoteSDP(med *DialogMedia, remoteSDP []byte) error {
	sess := med.mediaSession

	// Apply SDP on existing (Early) media if it exists
	if err := med.checkEarlyMedia(remoteSDP); !errors.Is(err, ErrMediaSessionMissing) {
		return err
	}

	if err := sess.RemoteSDP(remoteSDP); err != nil {
		return err
	}

	// Create RTP session. After this no media session configuration should be changed
	rtpSess := media.NewRTPSession(sess)
	med.mu.Lock()
	med.initRTPSessionUnsafe(sess, rtpSess)
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	med.mu.Unlock()

	// Must be called after reader and writer setup due to race
	return rtpSess.MonitorBackground()
}

// Ack acknowledgeds media
// Before Ack normally you want to setup more stuff like bridging
// Options allow customizing headers of the ACK request.
// Scope: WithHeaders, WithContact, WithRequestMutator.
func (d *DialogClientSession) Ack(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Ack", scopeClientAck)
	if err != nil {
		return err
	}

	inviteRequest := d.InviteRequest
	recipient := inviteRequest.Recipient
	if contact := d.InviteResponse.Contact(); contact != nil {
		recipient = contact.Address
	}

	if err := d.ack(ctx, recipient, nil, params); err != nil {
		return err
	}

	// NOTE it generally advisable todo this after successfull ACK:
	// Server may not even listen yet as it is waiting for ACK
	if ms := d.MediaSession(); ms != nil {
		if err := ms.Finalize(); err != nil {
			return err
		}
	}

	return nil
}

func (d *DialogClientSession) ack(ctx context.Context, remoteTarget sip.Uri, body []byte, params *signalParams) error {
	ackRequest := sip.NewRequest(
		sip.ACK,
		remoteTarget,
	)

	if body != nil {
		// This is delayed offer
		ackRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ackRequest.SetBody(body)
	}

	if err := applyRequestSignal(ackRequest, params); err != nil {
		return err
	}

	if err := d.DialogClientSession.WriteAck(ctx, ackRequest); err != nil {
		return err
	}
	return nil
}

// ReInvite sends new invite based on current media session
// Options allow customizing Contact, headers, SDP body and to mutate the final request.
// Scope: WithHeaders, WithContact, WithBody, WithRequestMutator.
func (d *DialogClientSession) ReInvite(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "ReInvite", scopeReInvite)
	if err != nil {
		return err
	}

	d.mu.Lock()
	if d.mediaSession == nil {
		d.mu.Unlock()
		return ErrDialogNotAnswered
	}
	sdpBody := d.mediaSession.LocalSDP()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	if err := buildReInviteRequest(req, sdpBody, d.InviteRequest.Contact(), params); err != nil {
		return err
	}

	// reInviteExchange already sends the ACK. Do not append a second
	// ACK here — an earlier revision did and shipped two same-CSeq,
	// different-branch ACKs per re-INVITE.
	_, err = reInviteExchange(ctx, d, req)
	return err
}

// writeReInviteACK sends the final ACK of a re-INVITE exchange without
// options; see reInviteExchange.
func (d *DialogClientSession) writeReInviteACK(ctx context.Context, target sip.Uri) error {
	return d.ack(ctx, target, nil, nil)
}

// reInviteMediaSession updates with full new media session
// media MUST BE Forked
func (d *DialogClientSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *signalParams) error {
	sdpBody := ms.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		sdpBody = params.Msg.Body
	}

	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdpBody)
	if err := applyRequestSignal(req, params); err != nil {
		return err
	}

	res, err := reInviteExchange(ctx, d, req)
	if err != nil {
		return err
	}

	// Save new remote target contact and update media. A malformed 2xx
	// without Contact was tolerated by reInviteExchange (ACK fallback) — keep
	// the current target instead of clobbering it with nil.
	return func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if cont := res.Contact(); cont != nil {
			d.remoteContactTarget = cont
		}

		remoteSDP := res.Body()
		if err := ms.RemoteSDP(remoteSDP); err != nil {
			return fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
		}

		return d.mediaUpdateUnsafe(ms)
	}()
}

// reInvites withs empty SDP are way to keep alive or do some post media update after receiving offer on 2xx
func (d *DialogClientSession) reInviteKeepAlive(ctx context.Context) error {
	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())

	res, err := reInviteExchange(ctx, d, req)
	if err != nil {
		return err
	}

	// Save new remote target contact (keep current one if the 2xx came
	// without Contact — see reInviteExchange)
	d.mu.Lock()
	if cont := res.Contact(); cont != nil {
		d.remoteContactTarget = cont
	}
	d.mu.Unlock()

	return nil
}

// Refer tries todo refer (blind transfer) on call.
// Options allow customizing headers of the REFER request and receiving the
// transfer status via WithOnReferNotify.
// Scope: WithHeaders, WithContact, WithRequestMutator, WithOnReferNotify.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogClientSession) Refer(ctx context.Context, referTo sip.Uri, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Refer", scopeRefer)
	if err != nil {
		return err
	}
	// Guard before dereferencing InviteResponse: it is nil until the first
	// response is received (dialogRefer's own check would run too late).
	if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
		return fmt.Errorf("can only be called on answered dialog")
	}
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	var localContact *sip.ContactHeader
	if d.InviteResponse != nil {
		localContact = d.InviteResponse.Contact()
	}
	if params.Dialog.OnReferNotify != nil {
		d.onReferNotify = params.Dialog.OnReferNotify
	}
	d.mu.Unlock()
	return d.referWithContacts(ctx, cont, localContact, referTo, params)
}

// referWithContacts validates the REFER targets and starts the exchange. A
// malformed 2xx without Contact leaves the remote target empty (R3 keeps the
// previous target only when one was seen); dereferencing it used to panic.
// Referred-By falls back to the Contact of our own INVITE when the answer
// carried none.
func (d *DialogClientSession) referWithContacts(ctx context.Context, cont, localContact *sip.ContactHeader, referTo sip.Uri, params *signalParams) error {
	if cont == nil {
		return fmt.Errorf("can not refer: remote contact is missing")
	}
	if localContact == nil && d.InviteRequest != nil {
		localContact = d.InviteRequest.Contact()
	}
	if localContact == nil {
		return fmt.Errorf("can not refer: local contact is missing")
	}
	return dialogRefer(ctx, d, cont.Address, referTo, localContact.Address, params)
}

func (d *DialogClientSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
	dialogHandleReferNotify(d, req, tx)
}

func (d *DialogClientSession) handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	dialogHandleRefer(d, dg, req, tx, onRefDialog)
}

func (d *DialogClientSession) handleReInvite(req *sip.Request, tx sip.ServerTransaction) error {
	if err := d.ReadRequest(req, tx); err != nil {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request - "+err.Error(), nil))
	}

	return d.handleMediaUpdate(req, tx, d.InviteRequest.Contact())
}

func (d *DialogClientSession) handleReInviteACK(req *sip.Request, tx sip.ServerTransaction) error {
	// Check do we need to handle Late Offer from ACK and update media
	body := req.Body()
	if body != nil {
		// Update media session state under lock, but invoke the app callback after unlock to avoid deadlocks.
		d.mu.Lock()
		err := d.sdpUpdateUnsafe(body)
		onMediaUpdate := d.onMediaUpdate
		d.mu.Unlock()
		if err != nil {
			return err
		}

		if onMediaUpdate != nil {
			onMediaUpdate(d.DialogMedia())
		}
	}

	// Read via locked getter: sdpUpdateUnsafe above may have replaced the media
	// session, and a BYE/re-INVITE on another transaction goroutine writes it too
	if ms := d.MediaSession(); ms != nil {
		return ms.Finalize()
	}
	return nil
}

func (d *DialogClientSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	return readSIPInfoDTMF(&d.dialogMedia, req, tx)
}

// Hold puts dialog on hold (media sendonly). Options allow customizing the re-INVITE.
// Scope: WithHeaders, WithContact, WithBody, WithRequestMutator, WithMusicOnHold.
// WithMusicOnHold selects the hold music started automatically after the
// re-INVITE succeeds (falling back to MediaConfig.MusicOnHold); other media
// overrides are rejected.
//
// The automatic music runs detached from the Hold call's context (which
// typically carries the re-INVITE timeout) until Unhold, Stop/StopMusicOnHold,
// or dialog Close; when the dialog-level default is unset (no MusicOnHold
// configured anywhere) Hold behaves as before and plays nothing.
func (d *DialogClientSession) Hold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Hold", scopeHold)
	if err != nil {
		return err
	}
	ms := d.MediaSession()
	if ms == nil {
		return ErrDialogNotAnswered
	}
	m := ms.Fork()
	m.Mode = sdp.ModeSendonly
	if err := d.reInviteMediaSession(ctx, m, params); err != nil {
		return err
	}
	d.mohAutoStart(ctx, params.Media.MusicOnHold)
	return nil
}

// Unhold takes dialog back from hold (media sendrecv). Options allow customizing the re-INVITE.
// Scope: WithHeaders, WithContact, WithBody, WithRequestMutator. The music
// started automatically by Hold is stopped on success; media options are rejected.
func (d *DialogClientSession) Unhold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Unhold", scopeUnhold)
	if err != nil {
		return err
	}
	ms := d.MediaSession()
	if ms == nil {
		return ErrDialogNotAnswered
	}
	m := ms.Fork()
	m.Mode = sdp.ModeSendrecv
	if err := d.reInviteMediaSession(ctx, m, params); err != nil {
		return err
	}
	d.mohAutoStop()
	return nil
}
