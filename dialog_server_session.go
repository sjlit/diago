// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	mrand "math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type OnReferDialogFunc func(referDialog *DialogClientSession) error

// DialogServerSession represents inbound channel
type DialogServerSession struct {
	*sipgo.DialogServerSession

	// MediaSession *media.MediaSession
	DialogMedia

	onReferDialog OnReferDialogFunc

	mediaConf MediaConfig
	closed    atomic.Uint32
}

func (d *DialogServerSession) Id() string {
	return d.ID
}

// Close frees local resources (media stack and dialog cleanup hooks).
// It is idempotent and does NOT send any SIP message. Server dialogs are
// closed by the framework when the serve handler returns
// (docs/contracts.md §6).
func (d *DialogServerSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.DialogMedia.Close()
	e2 := d.DialogServerSession.Close()
	return errors.Join(e1, e2)
}

func (d *DialogServerSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

// User that was dialed
func (d *DialogServerSession) ToUser() string {
	return d.InviteRequest.To().Address.User
}

// Authorize challenges and validates the incoming INVITE with SIP digest
// authentication (RFC 2617).
//
// Call it as the first thing in your serve handler:
//
//	dg.Serve(ctx, func(d *diago.DialogServerSession) error {
//		if err := d.Authorize(digestAuthServer, diago.DigestAuth{Username: "u", Password: "p"}); err != nil {
//			return err // 401 is sent; returning terminates the dialog
//		}
//		...
//	})
//
// On the first call the INVITE transaction is answered 401 Unauthorized with a
// WWW-Authenticate challenge and a non-nil error is returned; the caller must
// re-INVITE with an Authorization header. Failed validation is answered the
// same way inside the transaction (401 carrying a fresh challenge for bad
// credentials or unknown/expired nonces, 400/403 for malformed credentials)
// and also returns a non-nil error. Successful validation sends no
// response - dialog processing (Trying, Ringing, Answer) continues normally.
func (d *DialogServerSession) Authorize(s *DigestAuthServer, auth DigestAuth) error {
	return s.AuthorizeDialog(d, auth)
}

func (d *DialogServerSession) Transport() string {
	return d.InviteRequest.Transport()
}

// respondSignal builds a response for the dialog INVITE applying signaling
// options (Contact replace, extra headers, response mutator) and writes it.
// When the outgoing response has a body and no Content-Type header was
// provided, it defaults to "application/sdp". Methods that do not send a
// body (Trying, Ringing) pass body=nil and therefore never receive a
// Content-Type header.
func (d *DialogServerSession) respondSignal(statusCode int, reason string, body []byte, params *SignalParams) error {
	res := sip.NewResponseFromRequest(d.InviteRequest, statusCode, reason, body)
	if params != nil {
		if params.Msg.Contact != nil {
			setSignalContact(res, params.Msg.Contact)
		}
		applySignalHeaders(res, params.Msg.Headers)
		if body != nil && res.ContentType() == nil {
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		}
		if params.Msg.MutateResponse != nil {
			if err := params.Msg.MutateResponse(res); err != nil {
				return err
			}
		}
	}
	return d.DialogServerSession.WriteResponse(res)
}

// byeSignal sends BYE applying signaling options (extra headers, request mutator).
func (d *DialogServerSession) byeSignal(ctx context.Context, params *SignalParams) error {
	cont := d.InviteRequest.Contact()
	bye := sip.NewRequest(sip.BYE, cont.Address)
	bye.SetTransport(d.InviteRequest.Transport())

	if err := applyRequestSignal(bye, params); err != nil {
		return err
	}
	return d.DialogServerSession.WriteBye(ctx, bye)
}

// Trying sends 100 Trying.
// Honors: msg (Headers, Contact, MutateResponse); Body is ignored.
func (d *DialogServerSession) Trying(opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	return d.respondSignal(sip.StatusTrying, "Trying", nil, params)
}

// Progress sends 100 trying.
//
// Deprecated: Use Trying. It will change behavior to 183 Sesion Progress in future releases
func (d *DialogServerSession) Progress() error {
	return d.Trying()
}

// ProgressMedia sends 183 Session Progress and creates early media
//
// Honors: msg (Headers, Contact, Body, MutateResponse), media (all).
//
// Experimental: Naming of API might change
func (d *DialogServerSession) ProgressMedia(opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}

	rtpSess, err := d.setupProgressMedia(params)
	if err != nil {
		return err
	}

	body := rtpSess.Sess.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}
	if err := d.respondSignal(sip.StatusSessionInProgress, "Session Progress", body, params); err != nil {
		return err
	}
	return rtpSess.MonitorBackground()
}

// ProgressMediaOptions sends 183 Session Progress with options.
//
// Deprecated: Use ProgressMedia with SignalOptions
func (d *DialogServerSession) ProgressMediaOptions(opt ProgressMediaOptions) error {
	var opts []SignalOption
	if opt.Codecs != nil {
		opts = append(opts, WithCodecs(opt.Codecs...))
	}
	if opt.RTPNAT != 0 {
		opts = append(opts, WithRTPNAT(opt.RTPNAT))
	}
	return d.ProgressMedia(opts...)
}

// ProgressMediaOptions is the legacy option struct for ProgressMedia.
//
// Deprecated: Use ProgressMedia with SignalOptions.
type ProgressMediaOptions struct {
	// Codecs that will be used
	Codecs []media.Codec

	// RTPNAT exposes MediaSession property
	RTPNAT int
}

// setupProgressMedia initializes (early) media session for 183 response.
func (d *DialogServerSession) setupProgressMedia(params *SignalParams) (*media.RTPSession, error) {
	sess := params.Media.MediaSession
	if sess == nil {
		conf := signalMediaConfig(d.mediaConf, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return nil, err
		}
		sess = d.mediaSession
	}
	rtpSess := media.NewRTPSession(sess)
	if err := d.setupRTPSession(rtpSess); err != nil {
		return nil, err
	}
	return rtpSess, nil
}

// Ringing sends 180 Ringing.
// Honors: msg (Headers, Contact, MutateResponse); Body is ignored.
func (d *DialogServerSession) Ringing(opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	return d.respondSignal(sip.StatusRinging, "Ringing", nil, params)
}

func (d *DialogServerSession) DialogSIP() *sipgo.Dialog {
	return &d.Dialog
}

func (d *DialogServerSession) RemoteContact() *sip.ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.remoteContactTarget != nil {
		return d.remoteContactTarget
	}
	return d.InviteRequest.Contact()
}

// RespondSDP responds with 200 OK and provided SDP body.
// Options can customize status headers and Contact of the response.
// Honors: msg (Headers, Contact, MutateResponse); Body is ignored (use the body argument).
func (d *DialogServerSession) RespondSDP(body []byte, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	return d.respondSignal(sip.StatusOK, "OK", body, params)
}

// Answer creates media session and answers
// After this new AudioReader and AudioWriter are created for audio manipulation
// Options allow customizing Contact, headers, SDP body and media of the 200 OK response.
// Honors: msg (Headers, Contact, Body, MutateResponse), dialog (OnMediaUpdate,
// OnRefer); media overrides only apply when Answer creates the media session
// itself (no early media from ProgressMedia).
// NOTE: Not final API
func (d *DialogServerSession) Answer(opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if params != nil {
		if params.Dialog.OnRefer != nil {
			d.onReferDialog = params.Dialog.OnRefer
		}
		if params.Dialog.OnMediaUpdate != nil {
			d.onMediaUpdate = params.Dialog.OnMediaUpdate
		}
	}
	sess := d.mediaSession
	d.mu.Unlock()

	// Media Exists as early
	if sess != nil {
		// This will now block until ACK received with 64*T1 as max.
		body := sess.LocalSDP()
		if params != nil && params.Msg.Body != nil {
			body = params.Msg.Body
		}
		if err := d.respondSignal(sip.StatusOK, "OK", body, params); err != nil {
			return err
		}

		// Early media session was created by ProgressMedia and never finalized.
		// Finalize runs deferred media setup like DTLS handshake, same as answerSession.
		// MonitorBackground is already started by ProgressMedia and must not be started twice.
		return sess.Finalize()
	}

	rtpSess, err := d.newAnswerRTPSession(params)
	if err != nil {
		return err
	}
	return d.answerSession(rtpSess, params)
}

// newAnswerRTPSession creates RTP session for answering. It honors custom
// media session passed with options, otherwise builds one from media config.
func (d *DialogServerSession) newAnswerRTPSession(params *SignalParams) (*media.RTPSession, error) {
	sess := params.Media.MediaSession
	if sess == nil {
		conf := signalMediaConfig(d.mediaConf, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return nil, err
		}
		sess = d.mediaSession
	}
	return media.NewRTPSession(sess), nil
}

// AnswerOptions is the legacy option struct for Answer.
//
// Deprecated: Use Answer with SignalOptions.
type AnswerOptions struct {
	// OnMediaUpdate triggers when media update happens. It is blocking func, so make sure you exit
	OnMediaUpdate func(d *DialogMedia)

	// OnRefer is called on successfull REFER handling
	//
	// It creates new dialog (NewDialog) on which you need to call Invite() and Ack()
	// Any error from invite, ack or other processing should be returned for correct Notify handling
	//
	// NOTE: IT is SCOPED to handler and exiting handler will Close/Terminate this dialog!
	OnRefer func(referDialog *DialogClientSession) error
	// Codecs that will be used
	Codecs []media.Codec

	// RTPNAT is media.MediaSession.RTPNAT
	// Check media.RTPNAT... options
	RTPNAT int
}

// AnswerOptions allows to answer dialog with options
//
// Deprecated: Use Answer with SignalOptions
func (d *DialogServerSession) AnswerOptions(opt AnswerOptions) error {
	var opts []SignalOption
	if opt.OnMediaUpdate != nil {
		opts = append(opts, WithOnMediaUpdate(opt.OnMediaUpdate))
	}
	if opt.OnRefer != nil {
		opts = append(opts, WithOnRefer(opt.OnRefer))
	}
	if opt.Codecs != nil {
		opts = append(opts, WithCodecs(opt.Codecs...))
	}
	if opt.RTPNAT != 0 {
		opts = append(opts, WithRTPNAT(opt.RTPNAT))
	}
	return d.Answer(opts...)
}

// answerSession. It allows answering with custom RTP Session.
// NOTE: Not final API
func (d *DialogServerSession) answerSession(rtpSess *media.RTPSession, params *SignalParams) error {
	// TODO: Use setupRTPSession
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in INVITE")
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()

	body := sess.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}

	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSignal(sip.StatusOK, "OK", body, params); err != nil {
		return err
	}

	if err := sess.Finalize(); err != nil {
		return err
	}
	// fmt.Println("--------SErver finalized")

	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

func (d *DialogServerSession) setupRTPSession(rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in INVITE")
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()
	return nil
}

// AnswerLate does answer with Late offer.
// Options allow customizing Contact, headers, SDP body and media of the 200 OK response.
// Honors: msg (Headers, Contact, Body, MutateResponse), media (all).
func (d *DialogServerSession) AnswerLate(opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}

	rtpSess, err := d.newAnswerRTPSession(params)
	if err != nil {
		return err
	}
	sess := rtpSess.Sess
	localSDP := sess.LocalSDP()

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()

	body := localSDP
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}
	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSignal(sip.StatusOK, "OK", body, params); err != nil {
		return err
	}
	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

func (d *DialogServerSession) ReadAck(req *sip.Request, tx sip.ServerTransaction) error {
	// Check do we have some session
	err := func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		sess := d.mediaSession
		if sess == nil {
			return nil
		}
		contentType := req.ContentType()
		if contentType == nil {
			return nil
		}
		body := req.Body()
		if body != nil && contentType.Value() == "application/sdp" {
			// This is Late offer response
			if err := sess.RemoteSDP(body); err != nil {
				return err
			}
			d.updateRemoteHeldUnsafe()

			// Finalize session
			if err := sess.Finalize(); err != nil {
				return nil
			}
		}
		return nil
	}()
	if err != nil {
		e := d.Hangup(d.Context())
		return errors.Join(err, e)
	}

	return d.DialogServerSession.ReadAck(req, tx)
}

// Hangup terminates dialog. When dialog is confirmed BYE is sent,
// otherwise the INVITE is declined with 480 (and nil is returned — declining
// succeeded). Options allow customizing headers (ex. Reason), Contact and body
// of the outgoing message. See docs/contracts.md §7 for the full matrix.
// Honors: msg (Headers, Contact, MutateRequest on BYE, MutateResponse on 480 decline);
// Body is ignored.
func (d *DialogServerSession) Hangup(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
	if err != nil {
		return err
	}

	state := d.LoadState()
	if state >= sip.DialogStateConfirmed {
		return d.byeSignal(ctx, params)
	}
	return d.respondSignal(sip.StatusTemporarilyUnavailable, "Temporarly unavailable", nil, params)
}

// ReInvite sends a re-INVITE with the current media session.
// Honors: msg (Headers, Contact, Body, MutateRequest); media overrides are not consumed.
func (d *DialogServerSession) ReInvite(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
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
	if err := buildReInviteRequest(req, sdpBody, nil, params); err != nil {
		return err
	}

	res, err := d.Do(ctx, req)
	if err != nil {
		return err
	}

	if !res.IsSuccess() {
		return sipgo.ErrDialogResponse{
			Res: res,
		}
	}

	cont := res.Contact()
	if cont == nil {
		return fmt.Errorf("reinvite: no contact header present")
	}

	ack := sip.NewRequest(sip.ACK, cont.Address)
	return d.WriteRequest(ack)
}

// reInviteMediaSession updates with full new media session
// media MUST BE Forked
func (d *DialogServerSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *SignalParams) error {
	sdpBody := ms.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		sdpBody = params.Msg.Body
	}

	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	if params != nil && params.Msg.Contact != nil {
		setSignalContact(req, params.Msg.Contact)
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

func (d *DialogServerSession) reInviteDo(ctx context.Context, req *sip.Request) (*sip.Response, error) {

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

		// ACK the 2xx at its Contact (RFC 3261 §13.2.1). A 2xx without Contact
		// is malformed but does occur in the wild — dereferencing it here used
		// to panic. Fall back to the dialog's remote target.
		ackContact := res.Contact()
		if ackContact == nil {
			ackContact = d.RemoteContact()
		}
		if ackContact == nil {
			return nil, fmt.Errorf("reinvite: 2xx has no Contact and dialog has no remote target to ACK")
		}
		if err := d.ack(ctx, ackContact.Address, nil); err != nil {
			return res, err
		}

		return res, nil
	}
}

func (d *DialogServerSession) ack(ctx context.Context, remoteTarget sip.Uri, body []byte) error {
	// inviteRequest := d.InviteRequest
	// recipient := &inviteRequest.Recipient
	// if contact := d.InviteResponse.Contact(); contact != nil {
	// 	recipient = &contact.Address
	// }
	ackRequest := sip.NewRequest(
		sip.ACK,
		remoteTarget,
	)

	if body != nil {
		// This is delayed offer
		ackRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ackRequest.SetBody(body)
	}

	if err := d.DialogServerSession.WriteRequest(ackRequest); err != nil {
		return err
	}

	// if err := d.DialogServerSession.WriteAck(ctx, ackRequest); err != nil {
	// 	return err
	// }

	// Now dialog is established and can be add into store
	// if err := DialogsClientCache.DialogStore(ctx, d.ID, d); err != nil {
	// 	return err
	// }
	// d.OnClose(func() error {
	// 	return DialogsClientCache.DialogDelete(context.Background(), d.ID)
	// })
	return nil
}

func (d *DialogServerSession) remoteContactUnsafe() *sip.ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	return d.InviteRequest.Contact()
}

// Refer tries todo refer (blind transfer) on call. For more control use ReferOptions
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogServerSession) Refer(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	// cont := d.InviteRequest.Contact()
	// return dialogRefer(ctx, d, cont.Address, referTo, headers...)
	return d.ReferOptions(ctx, referTo, ReferServerOptions{
		Headers: headers,
	})
}

type ReferServerOptions struct {
	Headers  []sip.Header
	OnNotify func(statusCode int)
}

func (d *DialogServerSession) ReferOptions(ctx context.Context, referTo sip.Uri, opts ReferServerOptions) error {
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if opts.OnNotify != nil {
		d.onReferNotify = opts.OnNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, opts.Headers...)
}

func (d *DialogServerSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
	dialogHandleReferNotify(d, req, tx)
}

func (d *DialogServerSession) handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	dialogHandleRefer(d, dg, req, tx, onRefDialog)
}

func (d *DialogServerSession) handleReInvite(req *sip.Request, tx sip.ServerTransaction) error {
	// Check is current pending dialog
	if state := d.LoadState(); state == sip.DialogStateEstablished {
		// RFC 3261 §14.2 — UAS Behavior
		// If a UAS receives an INVITE request for an existing dialog while another INVITE transaction is in progress, it MUST return a 491 (Request Pending) response to the new INVITE.”
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
	}

	// NOTE: Calling ReadRequest increases remote CSEQ.
	// We should not call this until dialog is confirmed, otherwise any intermidiate response
	// will have wrong CSEQ
	if err := d.ReadRequest(req, tx); err != nil {
		if errors.Is(err, sipgo.ErrDialogInvalidCseq) {
			// https://datatracker.ietf.org/doc/html/rfc3261#section-14.2
			// 			A UAS that receives a second INVITE before it sends the final
			//    response to a first INVITE with a lower CSeq sequence number on the
			//    same dialog MUST return a 500 (Server Internal Error)  response to the
			//    second INVITE and MUST include a Retry-After header field with a
			//    randomly chosen value of between 0 and 10 seconds.
			res := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
			res.AppendHeader(sip.NewHeader("Retry-After", strconv.Itoa(rand.IntN(10))))
			return tx.Respond(res)
		}

		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
	}

	return d.handleMediaUpdate(req, tx, d.InviteResponse.Contact())
}

func (d *DialogServerSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	return readSIPInfoDTMF(&d.DialogMedia, req, tx)
}

// Hold puts dialog on hold (media sendonly). Options allow customizing the re-INVITE.
// Honors: msg (Headers, Contact, Body, MutateRequest); media: WithMusicOnHold
// selects the hold music started automatically after the re-INVITE succeeds
// (falling back to MediaConfig.MusicOnHold); other media overrides are not consumed.
//
// The automatic music runs detached from the Hold call's context (which
// typically carries the re-INVITE timeout) until Unhold, Stop/StopMusicOnHold,
// or dialog Close; when the dialog-level default is unset (no MusicOnHold
// configured anywhere) Hold behaves as before and plays nothing.
func (d *DialogServerSession) Hold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
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
// Honors: msg (Headers, Contact, Body, MutateRequest); the music started
// automatically by Hold is stopped on success; other media overrides are not consumed.
func (d *DialogServerSession) Unhold(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts)
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
