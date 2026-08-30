// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

var (
	ErrClientEarlyMedia = errors.New("Early media detected")
)

// DialogClientSession represents outbound channel
type DialogClientSession struct {
	*sipgo.DialogClientSession

	DialogMedia

	onReferDialog OnReferDialogFunc
	mediaConfig   MediaConfig

	closed atomic.Uint32
}

func (d *DialogClientSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.DialogMedia.Close()
	e2 := d.DialogClientSession.Close()
	return errors.Join(e1, e2)
}

func (d *DialogClientSession) Id() string {
	return d.ID
}

// Hangup terminates dialog with BYE.
// Options allow customizing headers of the BYE request (ex. Reason header),
// Contact and to mutate the final request.
func (d *DialogClientSession) Hangup(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	return d.byeSignal(ctx, params)
}

// byeSignal builds BYE request applying signaling options and writes it.
// Mandatory dialog headers (From/To/Call-ID/CSeq) are filled by sipgo.
func (d *DialogClientSession) byeSignal(ctx context.Context, params *SignalParams) error {
	if d.InviteResponse == nil {
		return fmt.Errorf("bye: can not send as no invite response present")
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

func (d *DialogClientSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

func (d *DialogClientSession) ToUser() string {
	return d.InviteRequest.To().Address.User
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

// InviteClientOptions is passed on dialog client Invite with extra control over dialog
//
// Deprecated: Use Invite with SignalOptions. Convert existing struct with Options()
type InviteClientOptions struct {
	Originator DialogSession
	OnResponse func(res *sip.Response) error
	// OnMediaUpdate called when media is changed.
	// NOTE: you should not block this call as it blocks response processing.
	OnMediaUpdate func(d *DialogMedia)
	// OnRefer is called on successfull REFER handling
	//
	// It creates new dialog (NewDialog) on which you need to call Invite() and Ack()
	// Any error from invite, ack or other processing should be returned for correct Notify handling
	//
	// NOTE: IT is SCOPED to handler and exiting handler will Close/Terminate this dialog!
	OnRefer OnReferDialogFunc
	// For digest authentication
	Username string
	Password string

	// Custom headers to pass. DO NOT SET THIS to nil
	Headers []sip.Header
	// Stop on early media. ErrClientEarlyMedia will be returned
	EarlyMediaDetect bool
}

// Options converts legacy options into SignalOptions
func (o *InviteClientOptions) Options() ([]SignalOption, error) {
	var opts []SignalOption
	if o.Originator != nil {
		opts = append(opts, WithOriginator(o.Originator))
	}
	if o.OnResponse != nil {
		opts = append(opts, WithOnResponse(o.OnResponse))
	}
	if o.OnMediaUpdate != nil {
		opts = append(opts, WithOnMediaUpdate(o.OnMediaUpdate))
	}
	if o.OnRefer != nil {
		opts = append(opts, WithOnRefer(o.OnRefer))
	}
	if o.Username != "" || o.Password != "" {
		opts = append(opts, WithAuthCredentials(o.Username, o.Password))
	}
	if len(o.Headers) > 0 {
		opts = append(opts, WithHeaders(o.Headers...))
	}
	if o.EarlyMediaDetect {
		opts = append(opts, WithEarlyMediaDetect())
	}
	return opts, nil
}

// WithAnonymousCaller sets from user Anonymous per RFC
//
// Deprecated: Use Invite with WithHeaders and sip.FromHeader
func (o *InviteClientOptions) WithAnonymousCaller() {
	o.Headers = append(o.Headers, &sip.FromHeader{
		DisplayName: "Anonymous",
		Address:     sip.Uri{User: "anonymous", Host: "anonymous.invalid"},
		Params:      sip.NewParams(),
	})
}

// WithCaller allows simpler way modifying caller
//
// Deprecated: Use Invite with WithHeaders and sip.FromHeader
func (o *InviteClientOptions) WithCaller(displayName string, callerID string, host string) {
	o.Headers = append(o.Headers, &sip.FromHeader{
		DisplayName: displayName,
		Address:     sip.Uri{User: callerID, Host: host},
		Params:      sip.NewParams(),
	})
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
// Errors:
// - sipgo.ErrDialogResponse
// - ErrClientEarlyMedia
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
// If you pass originator it will use originator to set correct from header and avoid media transcoding
func (d *DialogClientSession) Invite(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	return d.inviteWithParams(ctx, params)
}

// inviteWithParams performs the Invite flow using a pre-computed *SignalParams.
// It exists so callers (e.g. InviteBridge) can compute params once and avoid
// running user-supplied SignalOption funcs twice.
func (d *DialogClientSession) inviteWithParams(ctx context.Context, params *SignalParams) error {
	if params == nil || params.MediaSession == nil {
		conf := signalMediaConfig(d.mediaConfig, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return err
		}
	} else if d.mediaSession == nil {
		// Use custom pre-created media session. When combined with an
		// Originator we must Fork the session: the originator branch in
		// invite() rewrites Codecs and applies RemoteSDP/SetRemoteAddr, and
		// we don't want to mutate a session that the caller may reuse.
		sess := params.MediaSession
		if params.Originator != nil {
			sess = params.MediaSession.Fork()
		}
		d.mu.Lock()
		if d.mediaSession == nil {
			d.initMediaSessionUnsafe(sess, nil, nil)
		}
		d.mu.Unlock()
	}
	return d.invite(ctx, &d.DialogMedia, params)
}

func (d *DialogClientSession) invite(ctx context.Context, med *DialogMedia, params *SignalParams) error {
	sess := med.mediaSession
	inviteReq := d.InviteRequest
	originator := params.Originator

	// Custom headers must be applied before originator logic so it can
	// detect user provided From header
	applySignalHeaders(inviteReq, params.Headers)

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
	if params.Contact != nil {
		setSignalContact(inviteReq, params.Contact)
	} else {
		inviteReq.AppendHeader(&dialogCli.ContactHDR)
	}

	body := sess.LocalSDP()
	if params.Body != nil {
		body = params.Body
	}
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody(body)

	// We allow changing full from header, but we need to make sure it is correctly set
	// If users specify 'tag' parameter it is assumed that they know what they do
	if fromHDR := inviteReq.From(); fromHDR != nil && !fromHDR.Params.Has("tag") {
		fromHDR.Params.Add("tag", sip.GenerateTagN(16))
	}

	// Last chance for full customization of the request
	if params.MutateRequest != nil {
		if err := params.MutateRequest(inviteReq); err != nil {
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
	med.onMediaUpdate = params.OnMediaUpdate
	d.onReferDialog = params.OnRefer
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
	ansOpts := sipgo.AnswerOptions{
		Username:   params.Username,
		Password:   params.Password,
		OnResponse: params.OnResponse,
	}

	if params.EarlyMediaDetect {
		return d.waitAnswerEarly(ctx, med, ansOpts)
	}

	return d.waitAnswer(ctx, med, ansOpts)
}

// WaitAnswer waits dialog on answer. It should only be used if you have error Invite but still want to continue
// ex. ErrClientEarlyMedia was returned but you want to proceed with answering
func (d *DialogClientSession) WaitAnswer(ctx context.Context, opts sipgo.AnswerOptions) error {
	return d.waitAnswer(ctx, &d.DialogMedia, opts)
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
		return fmt.Errorf("no SDP in response")
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
	if err := med.checkEarlyMedia(remoteSDP); err != errNoRTPSession {
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
func (d *DialogClientSession) Ack(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
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

// AckLate sends ACK with media. Use this in combination with late(delay) offer
// func (d *DialogClientSession) AckLate(ctx context.Context) error {
// 	return d.ack(ctx, d.mediaSession.LocalSDP())
// }

func (d *DialogClientSession) ack(ctx context.Context, remoteTarget sip.Uri, body []byte, params *SignalParams) error {
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
func (d *DialogClientSession) ReInvite(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}

	d.mu.Lock()
	if d.mediaSession == nil {
		d.mu.Unlock()
		return errors.New("dialog session not answered")
	}
	sdpBody := d.mediaSession.LocalSDP()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	if err := buildReInviteRequest(req, sdpBody, d.InviteRequest.Contact(), params); err != nil {
		return err
	}

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	cont := res.Contact()
	if cont == nil {
		return fmt.Errorf("no contact header present")
	}

	ack := sip.NewRequest(sip.ACK, cont.Address)
	return d.WriteRequest(ack)
}

func (d *DialogClientSession) reInviteDo(ctx context.Context, req *sip.Request) (*sip.Response, error) {

	for {
		res, err := d.Do(ctx, req.Clone())
		if err != nil {
			return nil, err
		}

		if !res.IsSuccess() {
			// https://datatracker.ietf.org/doc/html/rfc3261#section-14.1
			// If a UAC receives a 491 response to a re-INVITE, it SHOULD start a
			//    timer with a value T chosen as follows:
			//       1. If the UAC is the owner of the Call-ID of the dialog ID
			//          (meaning it generated the value), T has a randomly chosen value
			//          between 2.1 and 4 seconds in units of 10 ms.

			//       2. If the UAC is not the owner of the Call-ID of the dialog ID, T
			//          has a randomly chosen value of between 0 and 2 seconds in units
			//          of 10 ms.

			if res.StatusCode == sip.StatusRequestPending {
				select {
				case <-time.After(time.Duration(2000+mrand.IntN(200)*10) * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			return nil, sipgo.ErrDialogResponse{
				Res: res,
			}
		}

		// Now do ACK on new Contact
		if err := d.ack(ctx, res.Contact().Address, nil, nil); err != nil {
			return res, err
		}

		return res, nil
	}
}

// reInviteMediaSession updates with full new media session
// media MUST BE Forked
func (d *DialogClientSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *SignalParams) error {
	sdpBody := ms.LocalSDP()
	if params != nil && params.Body != nil {
		sdpBody = params.Body
	}

	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	if params != nil && params.Contact != nil {
		setSignalContact(req, params.Contact)
	}
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdpBody)
	if err := applyRequestSignal(req, params); err != nil {
		return err
	}

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	// Save new remote target contact and update media
	return func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.remoteContactTarget = res.Contact()

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

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	// Save new remote target contact
	d.mu.Lock()
	d.remoteContactTarget = res.Contact()
	d.mu.Unlock()

	return nil
}

// Refer tries todo refer (blind transfer) on call. For more control use ReferOptions
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogClientSession) Refer(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	// cont := d.InviteRequest.Contact()
	// return dialogRefer(ctx, d, cont.Address, referTo, headers...)
	return d.ReferOptions(ctx, referTo, ReferClientOptions{
		Headers: headers,
	})
}

type ReferClientOptions struct {
	Headers []sip.Header
	// OnNotify sends notify status code.
	// If implemented you need to react on different status code.
	OnNotify func(statusCode int)
}

func (d *DialogClientSession) ReferOptions(ctx context.Context, referTo sip.Uri, opts ReferClientOptions) error {
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if opts.OnNotify != nil {
		d.onReferNotify = opts.OnNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, opts.Headers...)
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

func (d *DialogClientSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
}

// Hold puts dialog on hold (media sendonly). Options allow customizing the re-INVITE.
func (d *DialogClientSession) Hold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	ms := d.MediaSession()
	if ms == nil {
		return errors.New("dialog session not answered")
	}
	m := ms.Fork()
	m.Mode = sdp.ModeSendonly
	if err := d.reInviteMediaSession(ctx, m, params); err != nil {
		return err
	}
	return nil
}

// Unhold takes dialog back from hold (media sendrecv). Options allow customizing the re-INVITE.
func (d *DialogClientSession) Unhold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	ms := d.MediaSession()
	if ms == nil {
		return errors.New("dialog session not answered")
	}
	m := ms.Fork()
	m.Mode = sdp.ModeSendrecv
	if err := d.reInviteMediaSession(ctx, m, params); err != nil {
		return err
	}
	return nil
}
