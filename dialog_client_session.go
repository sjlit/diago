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
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/media/sdp"
)

// ErrClientEarlyMedia is returned by Invite with WithEarlyMediaDetect when a
// 183 Session Progress with SDP arrives: media is negotiated and WaitAnswer
// can continue the call.
var (
	ErrClientEarlyMedia = errors.New("early media detected")
)

// OutboundCall is an outbound (UAC) dialog created by Dial/DialURI or
// NewCall+Invite. The diago methods are the supported API surface; use SIP for
// the underlying sipgo dialog.
type OutboundCall struct {
	sess *sipgo.DialogClientSession

	callMedia

	onReferDialog OnReferFunc
	mediaConfig   MediaConfig

	closed atomic.Uint32
}

// Media returns the media half of the session. It is never nil.
func (d *OutboundCall) Media() *Media {
	return &d.callMedia
}

// Context returns the dialog lifetime context.
func (d *OutboundCall) Context() context.Context {
	return d.sess.Context()
}

// Do sends an in-dialog request and waits for its final response. It is the
// raw SIP escape hatch; prefer the diago methods for dialog operations.
func (d *OutboundCall) Do(ctx context.Context, req *Request) (*Response, error) {
	return d.sess.Do(ctx, req)
}

// release frees local resources without sending SIP.
func (d *OutboundCall) release() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	return d.releaseUnsafe()
}

// releaseUnsafe closes local resources after the caller won the closed CAS.
func (d *OutboundCall) releaseUnsafe() error {
	e1 := d.callMedia.close()
	e2 := d.sess.Close()
	return errors.Join(e1, e2)
}

// Close terminates the dialog (CANCEL/BYE or no-op depending on its state)
// and releases local resources. It is idempotent and honors ctx for the
// signaling part.
func (d *OutboundCall) Close(ctx context.Context, opts ...CloseOptions) error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	var errs []error
	if d.State() != CallStateEnded {
		errs = append(errs, d.hangup(ctx, opts...))
	}
	errs = append(errs, d.releaseUnsafe())
	return errors.Join(errs...)
}

// ID returns the dialog identifier (Call-ID, tags and tags-derived hash).
func (d *OutboundCall) ID() string {
	return d.sess.ID
}

// Hangup terminates dialog with BYE.
// Options allow customizing headers of the BYE request (ex. Reason header),
// Contact and to mutate the final request.
//
// Behavior by dialog state:
//   - no response received yet: error ErrDialogNotAnswered (cancel the Invite
//     context instead, which sends CANCEL)
//   - provisional response received (early dialog): BYE on the early dialog
//   - confirmed: BYE, waits for 200
//   - already ended: returns nil silently
//
// hangup ends the signaling; local resources are released with Close (the
// framework also releases them when the dialog ends).
//
// Scope: CloseOptions.
func (d *OutboundCall) hangup(ctx context.Context, opts ...CloseOptions) error {
	params, err := closeSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.byeSignal(ctx, params)
}

// byeSignal builds BYE request applying signaling options and writes it.
// Mandatory dialog headers (From/To/Call-ID/CSeq) are filled by sipgo.
func (d *OutboundCall) byeSignal(ctx context.Context, params *signalParams) error {
	if d.sess.InviteResponse == nil {
		return byeNotPossible()
	}
	inviteRequest := d.sess.InviteRequest
	recipient := inviteRequest.Recipient
	if contact := d.sess.InviteResponse.Contact(); contact != nil {
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
	return d.sess.WriteBye(ctx, bye)
}

// byeNotPossible is returned when no BYE can be sent because the dialog was
// never established (no invite response received yet).
func byeNotPossible() error {
	return fmt.Errorf("bye: %w", ErrDialogNotAnswered)
}

// FromUser returns the caller user part of the dialog's From header.
func (d *OutboundCall) FromUser() string {
	return d.sess.InviteRequest.From().Address.User
}

// ToUser returns the callee user part of the dialog's To header.
func (d *OutboundCall) ToUser() string {
	return d.sess.InviteRequest.To().Address.User
}

// From returns the caller URI.
func (d *OutboundCall) From() URI {
	if d.sess.InviteRequest == nil || d.sess.InviteRequest.From() == nil {
		return URI{}
	}
	return d.sess.InviteRequest.From().Address
}

// To returns the callee URI.
func (d *OutboundCall) To() URI {
	if d.sess.InviteRequest == nil || d.sess.InviteRequest.To() == nil {
		return URI{}
	}
	return d.sess.InviteRequest.To().Address
}

// Transport returns the transport of the dialog's INVITE request (ex. "UDP").
func (d *OutboundCall) Transport() string {
	return d.sess.InviteRequest.Transport()
}

// SIP exposes the underlying sipgo dialog as the escape hatch.
func (d *OutboundCall) SIP() *Dialog {
	return &d.sess.Dialog
}

// RemoteContact returns the current remote target of the dialog.
func (d *OutboundCall) RemoteContact() *ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remoteContactUnsafe()
}

// remoteContactUnsafe returns the current remote target or nil when the dialog
// has no response yet (Invite failed or never completed) or the response
// carried no Contact. Callers must check for nil.
func (d *OutboundCall) remoteContactUnsafe() *ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	if d.sess.InviteResponse == nil {
		return nil
	}
	return d.sess.InviteResponse.Contact()
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
// Scope: InviteOptions.
//
// By default Invite returns after the final 2xx; call Ack yourself. Set
// InviteOptions.ACKPolicy to ACKPolicyWait to send the ACK before returning.
//
// Errors:
// - *DialogResponseError
// - ErrClientEarlyMedia
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
// If you pass originator it will use originator to set correct from header and avoid media transcoding
func (d *OutboundCall) Invite(ctx context.Context, opts ...InviteOptions) error {
	policy := inviteACKPolicy(opts...)
	params, err := inviteSignalParamsFrom(opts...)
	if err != nil {
		return err
	}
	if err := d.inviteWithParams(ctx, params); err != nil {
		return err
	}
	if policy == ACKPolicyWait {
		return d.Ack(ctx)
	}
	return nil
}

// inviteWithParams performs the Invite flow using a pre-computed *signalParams.
// It exists so callers (e.g. Agent.Dial, DialBridge) can compute params once
// and avoid running user-supplied SignalOption funcs twice.
func (d *OutboundCall) inviteWithParams(ctx context.Context, params *signalParams) error {
	if params == nil {
		return fmt.Errorf("invite: signal params are nil")
	}
	if params.Media.MediaSession == nil {
		conf := signalMediaConfig(d.mediaConfig, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return err
		}
	} else if d.mediaSession == nil {
		// Use custom pre-created media session. When combined with an
		// Originator we must Fork the session: the originator branch in
		// invite() rewrites Codecs and applies RemoteSDP/SetRemoteAddr, and
		// we don't want to mutate a session that the caller may reuse.
		conf := signalMediaConfig(d.mediaConfig, params)
		sess := params.Media.MediaSession
		if params.Dialog.Originator != nil {
			sess = params.Media.MediaSession.Fork()
		}
		d.mu.Lock()
		if d.callMedia.closed {
			d.mu.Unlock()
			return ErrDialogClosed
		}
		if d.mediaSession == nil {
			d.initMediaSessionUnsafe(sess, nil, nil)
			// Precreated sessions skip initMediaSessionFromConf; pick up the
			// dialog-level hold music here.
			d.mohTone = conf.MusicOnHold
		}
		d.mu.Unlock()
	}
	return d.invite(ctx, &d.callMedia, params)
}

func (d *OutboundCall) invite(ctx context.Context, med *Media, params *signalParams) error {
	sess := med.mediaSession
	inviteReq := d.sess.InviteRequest
	originator := params.Dialog.Originator

	// Custom headers must be applied before originator logic so it can
	// detect user provided From header
	applySignalHeaders(inviteReq, params.Msg.Headers)

	if originator != nil {
		// In case originator then:
		// - check do we support this media formats by conf
		// - if we do, then filter and pass to dial endpoint filtered
		origRaw, ok := originator.(RawCall)
		if !ok {
			return fmt.Errorf("originator: %w", ErrRawCallUnsupported)
		}
		origInvite := origRaw.SIP().InviteRequest
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

	dialogCli := d.sess.UA
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
	client := d.sess.UA.Client
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
	err := d.sess.Invite(ctx, func(c *Client, req *Request) error {
		// Do nothing
		return nil
	})
	if err != nil {
		return normalizeDialogResponseError(err)
	}
	ansOpts := answerOptionsFromParams(params)

	if params.Dialog.EarlyMediaDetect {
		return normalizeDialogResponseError(d.waitAnswerEarly(ctx, med, ansOpts))
	}

	return normalizeDialogResponseError(d.waitAnswer(ctx, med, ansOpts))
}

// answerOptionsFromParams extracts the sipgo-level answer options from parsed
// signalParams. It is the bridge between diago's SignalOption surface and the
// sipgo OutboundCall API.
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
// ErrClientEarlyMedia (or otherwise did not complete). Media options are not
// part of WaitAnswerOption: the media session was already negotiated by
// Invite. Options allow receiving responses (WithOnResponse) and digest
// credentials (WaitAnswerOptions.Username/Password).
// Scope: WaitAnswerOptions.
func (d *OutboundCall) WaitAnswer(ctx context.Context, opts ...WaitAnswerOptions) error {
	params, err := waitAnswerSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.waitAnswer(ctx, &d.callMedia, answerOptionsFromParams(params))
}

func (d *OutboundCall) waitAnswerEarly(ctx context.Context, med *Media, opts sipgo.AnswerOptions) error {
	sess := med.mediaSession
	onResps := opts.OnResponse

	// Add early media check
	opts.OnResponse = func(res *Response) error {
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
		ierr := med.initRTPSessionUnsafe(sess, rtpSess)
		if ierr == nil {
			med.onCloseUnsafe(func() error {
				return rtpSess.Close()
			})
		}
		med.mu.Unlock()
		if ierr != nil {
			return errors.Join(ierr, rtpSess.Close())
		}

		// Must be called after reader and writer setup due to race
		if err := rtpSess.MonitorBackground(); err != nil {
			return err
		}

		return ErrClientEarlyMedia
	}
	return d.waitAnswer(ctx, med, opts)
}

func (d *OutboundCall) waitAnswer(ctx context.Context, med *Media, opts sipgo.AnswerOptions) error {
	if err := d.sess.WaitAnswer(ctx, opts); err != nil {
		return normalizeDialogResponseError(err)
	}

	remoteSDP := d.sess.InviteResponse.Body()
	if remoteSDP == nil {
		return fmt.Errorf("no sdp in response: %w", ErrSDPMissing)
	}

	if err := d.applyRemoteSDP(med, remoteSDP); err != nil {
		// Terminate call. Call must be ACK before doing BYE
		ackErr := d.Ack(ctx)
		byeErr := d.sess.Bye(ctx)
		return errors.Join(err, ackErr, byeErr)
	}

	return nil
}

func (d *OutboundCall) applyRemoteSDP(med *Media, remoteSDP []byte) error {
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
	err := med.initRTPSessionUnsafe(sess, rtpSess)
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	med.mu.Unlock()
	if err != nil {
		return errors.Join(err, rtpSess.Close())
	}

	// Must be called after reader and writer setup due to race
	return rtpSess.MonitorBackground()
}

// Ack acknowledges media.
// Before Ack normally you want to setup more stuff like bridging.
// Scope: AckOptions.
func (d *OutboundCall) Ack(ctx context.Context, opts ...AckOptions) error {
	params, err := ackSignalParams(opts...)
	if err != nil {
		return err
	}

	inviteRequest := d.sess.InviteRequest
	recipient := inviteRequest.Recipient
	if contact := d.sess.InviteResponse.Contact(); contact != nil {
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

func (d *OutboundCall) ack(ctx context.Context, remoteTarget URI, body []byte, params *signalParams) error {
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

	if err := d.sess.WriteAck(ctx, ackRequest); err != nil {
		return err
	}
	return nil
}

// ReInvite sends new invite based on current media session
// Options allow customizing Contact, headers, SDP body and to mutate the final request.
// Scope: RequestOptions.
func (d *OutboundCall) ReInvite(ctx context.Context, opts ...RequestOptions) error {
	params, err := requestSignalParams(opts...)
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
	if contact == nil {
		return fmt.Errorf("reinvite: %w", ErrRemoteContactMissing)
	}

	req := sip.NewRequest(sip.INVITE, contact.Address)
	if err := buildReInviteRequest(req, sdpBody, d.sess.InviteRequest.Contact(), params); err != nil {
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
func (d *OutboundCall) writeReInviteACK(ctx context.Context, target URI) error {
	return d.ack(ctx, target, nil, nil)
}

// reInviteMediaSession updates with full new media session
// media MUST BE Forked
func (d *OutboundCall) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *signalParams) error {
	sdpBody := ms.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		sdpBody = params.Msg.Body
	}

	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()
	if contact == nil {
		return fmt.Errorf("reinvite media update: %w", ErrRemoteContactMissing)
	}

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.sess.InviteRequest.Contact())
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
			return fmt.Errorf("sdp update media remote sdp applying failed: %w", err)
		}

		return d.mediaUpdateUnsafe(ms)
	}()
}

// reInvites withs empty SDP are way to keep alive or do some post media update after receiving offer on 2xx
func (d *OutboundCall) reInviteKeepAlive(ctx context.Context) error {
	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()
	if contact == nil {
		return fmt.Errorf("reinvite keep-alive: %w", ErrRemoteContactMissing)
	}

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.sess.InviteRequest.Contact())

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
// transfer status via ReferOptions.OnReferNotify.
// Scope: ReferOptions.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *OutboundCall) Refer(ctx context.Context, referTo URI, opts ...ReferOptions) error {
	params, err := referSignalParams(opts...)
	if err != nil {
		return err
	}
	// Guard before dereferencing InviteResponse: it is nil until the first
	// response is received (dialogRefer's own check would run too late).
	if d.State() != CallStateConfirmed {
		return fmt.Errorf("refer: %w", ErrDialogNotAnswered)
	}
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	var localContact *ContactHeader
	if d.sess.InviteResponse != nil {
		localContact = d.sess.InviteResponse.Contact()
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
func (d *OutboundCall) referWithContacts(ctx context.Context, cont, localContact *ContactHeader, referTo URI, params *signalParams) error {
	if cont == nil {
		return fmt.Errorf("can not refer: remote contact is missing")
	}
	if localContact == nil && d.sess.InviteRequest != nil {
		localContact = d.sess.InviteRequest.Contact()
	}
	if localContact == nil {
		return fmt.Errorf("can not refer: local contact is missing")
	}
	return dialogRefer(ctx, d, cont.Address, referTo, localContact.Address, params)
}

func (d *OutboundCall) handleReferNotify(req *Request, tx ServerTransaction) {
	dialogHandleReferNotify(d, req, tx)
}

func (d *OutboundCall) handleRefer(dg *Agent, req *Request, tx ServerTransaction) {
	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		// The transaction may already be gone; there is no retry path here.
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	if err := dialogHandleRefer(d, dg, req, tx, onRefDialog); err != nil {
		dg.log.Error("Failed to handle REFER", "error", err)
	}
}

func (d *OutboundCall) handleReInvite(req *Request, tx ServerTransaction) error {
	if err := d.sess.ReadRequest(req, tx); err != nil {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request - "+err.Error(), nil))
	}

	return d.handleMediaUpdate(req, tx, d.sess.InviteRequest.Contact())
}

func (d *OutboundCall) handleReInviteACK(req *Request, tx ServerTransaction) error {
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
			onMediaUpdate(d.Media())
		}
	}

	// Read via locked getter: sdpUpdateUnsafe above may have replaced the media
	// session, and a BYE/re-INVITE on another transaction goroutine writes it too
	if ms := d.MediaSession(); ms != nil {
		return ms.Finalize()
	}
	return nil
}

func (d *OutboundCall) readSIPInfoDTMF(req *Request, tx ServerTransaction) error {
	return readSIPInfoDTMF(&d.callMedia, req, tx)
}

// Hold puts dialog on hold (media sendonly). Options allow customizing the re-INVITE.
// Scope: HoldOptions.
// HoldOptions.MusicOnHold selects the hold music started automatically after the
// re-INVITE succeeds (falling back to MediaConfig.MusicOnHold); other media
// overrides are rejected.
//
// The automatic music runs detached from the Hold call's context (which
// typically carries the re-INVITE timeout) until Unhold, Stop/StopMusicOnHold,
// or dialog Close; when the dialog-level default is unset (no MusicOnHold
// configured anywhere) Hold behaves as before and plays nothing.
func (d *OutboundCall) Hold(ctx context.Context, opts ...HoldOptions) error {
	params, err := holdSignalParams(opts...)
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
// Scope: RequestOptions. The music
// started automatically by Hold is stopped on success; media options are rejected.
func (d *OutboundCall) Unhold(ctx context.Context, opts ...RequestOptions) error {
	params, err := requestSignalParams(opts...)
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
