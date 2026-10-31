// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/media/sdp"
)

// OnReferFunc handles a REFER accepted by the dialog: it should establish
// the referred call on the provided dialog.
type OnReferFunc func(referDialog *OutboundCall) error

// InboundCall is an inbound (UAS) dialog delivered to the CallHandler given
// to Serve/Start. The diago methods are the supported API surface;
// use SIP for the underlying sipgo dialog.
type InboundCall struct {
	sess *sipgo.DialogServerSession

	// MediaSession *media.MediaSession
	callMedia

	onReferDialog OnReferFunc

	mediaConf MediaConfig
	closed    atomic.Uint32

	// ackCh is closed by diago's OnAck handler when the matching ACK
	// arrives. respondSignal selects on it as a defense against the
	// sipgo v1.4.3 WriteResponse "No ACK received" race: even when
	// sipgo's wait loop races past the state notification, the ACK has
	// been delivered to diago's OnAck path (which then calls readAck)
	// and we can rely on this channel as ground truth.
	//
	// ackMu guards the lazy creation and the one-shot close: ACK retransmits
	// can be dispatched concurrently and a bare check-then-close would panic
	// with "close of closed channel".
	ackMu   sync.Mutex
	ackCh   chan struct{}
	ackDone chan struct{}

	// ackResultMu guards ackErr, the failure readAck detected while processing
	// the ACK (media answer/finalize/confirm). respondSignal surfaces it so a
	// delivered ACK never masks a failed media setup.
	ackResultMu sync.Mutex
	ackErr      error

	// ackProcessed ensures the initial INVITE ACK is processed once; ACK
	// retransmits must not re-apply the media answer or re-run finalization.
	ackProcessed atomic.Bool

	// answerStarted serializes final 2xx responses: concurrent Answer,
	// AnswerLate and RespondSDP calls must not emit two 200s.
	answerStarted atomic.Bool
}

// Media returns the media half of the session. It is never nil.
func (d *InboundCall) Media() *Media {
	return &d.callMedia
}

// Context returns the dialog lifetime context.
func (d *InboundCall) Context() context.Context {
	return d.sess.Context()
}

// Do sends an in-dialog request and waits for its final response. It is the
// raw SIP escape hatch; prefer the diago methods for dialog operations.
func (d *InboundCall) Do(ctx context.Context, req *Request) (*Response, error) {
	return d.sess.Do(ctx, req)
}

// ackChannel lazily allocates and returns the per-dialog ACK-arrival channel.
// It is closed by diago's OnAck handler when the matching ACK arrives.
func (d *InboundCall) ackChannel() <-chan struct{} {
	ackCh, _ := d.ackChannels()
	return ackCh
}

// ackDoneChannel lazily allocates and returns the per-dialog ACK-processed
// channel. It is closed when readAck finished (media finalized or failed),
// or when the 2xx exchange ended without an ACK.
func (d *InboundCall) ackDoneChannel() <-chan struct{} {
	_, ackDone := d.ackChannels()
	return ackDone
}

// ackChannels lazily allocates the ACK-arrival and ACK-processed channels.
// ackCh is closed when the matching ACK arrives; ackDone when readAck finished
// processing it (including media finalization and failure teardown).
func (d *InboundCall) ackChannels() (<-chan struct{}, <-chan struct{}) {
	d.ackMu.Lock()
	defer d.ackMu.Unlock()
	if d.ackCh == nil {
		d.ackCh = make(chan struct{})
	}
	if d.ackDone == nil {
		d.ackDone = make(chan struct{})
	}
	return d.ackCh, d.ackDone
}

// setAckError records the first readAck failure. It must be called before the
// dialog advances to Confirmed so respondSignal observes it.
func (d *InboundCall) setAckError(err error) {
	if err == nil {
		return
	}
	d.ackResultMu.Lock()
	defer d.ackResultMu.Unlock()
	if d.ackErr == nil {
		d.ackErr = err
	}
}

// ackResult returns the recorded readAck failure, if any.
func (d *InboundCall) ackResult() error {
	d.ackResultMu.Lock()
	defer d.ackResultMu.Unlock()
	return d.ackErr
}

// notifyAckDone closes the ACK-processed channel, signaling that readAck
// finished. It is safe to call multiple times (subsequent calls are no-ops).
func (d *InboundCall) notifyAckDone() {
	d.ackMu.Lock()
	defer d.ackMu.Unlock()
	if d.ackDone == nil {
		d.ackDone = make(chan struct{})
	}
	select {
	case <-d.ackDone:
	default:
		close(d.ackDone)
	}
}

// notifyAck closes the dialog's ack channel, signaling any pending
// respondSignal waiters that the ACK has been delivered. It is safe to
// call multiple times (subsequent calls are no-ops).
func (d *InboundCall) notifyAck() {
	d.ackMu.Lock()
	defer d.ackMu.Unlock()
	if d.ackCh == nil {
		d.ackCh = make(chan struct{})
	}
	select {
	case <-d.ackCh:
		// already closed
	default:
		close(d.ackCh)
	}
}

// matchesInviteAck reports whether req acks the dialog's INVITE transaction
// (same CSeq number). Only such an ACK may close ackCh: a stray or forged ACK
// with another CSeq must not let a pending respondSignal treat the sipgo
// wait-loop timeout as success.
func (d *InboundCall) matchesInviteAck(req *Request) bool {
	if req == nil || d.sess.InviteRequest == nil {
		return false
	}
	reqCSeq, inviteCSeq := req.CSeq(), d.sess.InviteRequest.CSeq()
	if reqCSeq == nil || inviteCSeq == nil {
		return false
	}
	return reqCSeq.SeqNo == inviteCSeq.SeqNo
}

// ID returns the dialog identifier (Call-ID, tags and tags-derived hash).
func (d *InboundCall) ID() string {
	return d.sess.ID
}

// releaseUnsafe closes local resources after the caller won the closed CAS.
func (d *InboundCall) releaseUnsafe() error {
	e1 := d.callMedia.close()
	e2 := d.sess.Close()
	return errors.Join(e1, e2)
}

// Close terminates the dialog (decline, BYE or no-op depending on its state)
// and releases local resources. It is idempotent and honors ctx for the
// signaling part.
func (d *InboundCall) Close(ctx context.Context, opts ...CloseOptions) error {
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

// FromUser returns the caller user part of the dialog's From header.
func (d *InboundCall) FromUser() string {
	return d.sess.InviteRequest.From().Address.User
}

// ToUser returns the user that was dialed.
func (d *InboundCall) ToUser() string {
	return d.sess.InviteRequest.To().Address.User
}

// From returns the caller URI.
func (d *InboundCall) From() URI {
	if d.sess.InviteRequest == nil || d.sess.InviteRequest.From() == nil {
		return URI{}
	}
	return d.sess.InviteRequest.From().Address
}

// To returns the callee URI.
func (d *InboundCall) To() URI {
	if d.sess.InviteRequest == nil || d.sess.InviteRequest.To() == nil {
		return URI{}
	}
	return d.sess.InviteRequest.To().Address
}

// Authorize challenges and validates the incoming INVITE with SIP digest
// authentication (RFC 2617).
//
// Call it as the first thing in your serve handler:
//
//	dg.Serve(ctx, func(d *diago.InboundCall) error {
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
func (d *InboundCall) Authorize(s *DigestAuthServer, auth DigestAuth) error {
	return s.AuthorizeDialog(d, auth)
}

// Transport returns the transport of the dialog's INVITE (ex. "UDP").
func (d *InboundCall) Transport() string {
	return d.sess.InviteRequest.Transport()
}

// respondSignal builds a response for the dialog INVITE applying signaling
// options (Contact replace, extra headers, response mutator) and writes it.
// When the outgoing response has a body and no Content-Type header was
// provided, it defaults to "application/sdp". Methods that do not send a
// body (Trying, Ringing) pass body=nil and therefore never receive a
// Content-Type header.
//
// For final 2xx, this defends against a known sipgo v1.4.3 race: sipgo's
// WaitForAck loop can exit with "No ACK received" even though the ACK was
// delivered and processed by diago's OnAck/readAck path. We rely on a
// diago-managed ackCh closed by the OnAck handler as ground truth: if
// sipgo's loop races past the state notification, we treat the call as
// successful because the peer has acknowledged.
func (d *InboundCall) respondSignal(ctx context.Context, statusCode int, reason string, body []byte, params *signalParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res := sip.NewResponseFromRequest(d.sess.InviteRequest, statusCode, reason, body)
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

	// Only 2xx waits for ACK and is affected by the race.
	if !res.IsSuccess() {
		return d.sess.WriteResponse(res)
	}

	// The caller observes only initiation: waiting for the ACK (and
	// retransmitting the 2xx until it arrives) is confined to the background
	// so management operations and media setup are never blocked by a slow
	// peer.
	return d.writeSuccessAsync(res)
}

// writeSuccessAsync sends the final 2xx response and returns once it was
// handed to the transaction layer (dialog state Established), leaving 2xx
// retransmission and the ACK wait to a background goroutine. The ACK outcome
// is observable through WaitAck.
func (d *InboundCall) writeSuccessAsync(res *Response) error {
	switch state := d.sess.LoadState(); {
	case state == sip.DialogStateEnded:
		return ErrDialogEnded
	case state >= sip.DialogStateEstablished:
		// A final 2xx was already sent; a dialog can only be answered once.
		return fmt.Errorf("%w: state %s", ErrAlreadyAnswered, state)
	}
	// Serialize concurrent answers: the state check above can pass for two
	// goroutines before either write starts.
	if !d.answerStarted.CompareAndSwap(false, true) {
		return ErrAlreadyAnswered
	}

	// Register the state watcher before the write starts: sipgo marks the
	// dialog Established immediately before transmitting the response, so
	// observing that transition means the response is on its way. A later
	// Confirmed (ACK processed) covers the case where the ACK wins the race.
	stateCh := d.SIP().StateRead()
	writeDone := make(chan error, 1)
	go func() {
		err := d.sess.WriteResponse(res)
		writeDone <- err
		d.finishSuccessWrite(err)
	}()

	for {
		select {
		case st := <-stateCh:
			switch st {
			case sip.DialogStateEstablished, sip.DialogStateConfirmed:
				return nil
			case sip.DialogStateEnded:
				// CANCEL/teardown raced the response. Report the write result
				// when it is already known, otherwise the dialog end.
				select {
				case err := <-writeDone:
					if err != nil {
						return err
					}
				default:
				}
				return ErrDialogEnded
			}
		case err := <-writeDone:
			return err
		}
	}
}

// finishSuccessWrite records the outcome of the background 2xx exchange.
// It runs when sipgo's WriteResponse returns: on the ACK, on the 64*T1
// timeout (which sipgo reports as success) or on teardown.
func (d *InboundCall) finishSuccessWrite(err error) {
	// Known sipgo v1.4.3 race: the WaitForAck loop can exit with "No ACK
	// received" even though the ACK was delivered and processed by diago's
	// OnAck path. ackCh is ground truth for the ACK arrival.
	if err != nil && isAckWaitRaceError(err) {
		if waitAckOrGrace(context.Background(), d.ackChannel(), ackGraceWindow) {
			// Wait briefly for readAck to finish so a media failure it found
			// is recorded instead of the race.
			select {
			case <-d.ackDoneChannel():
			case <-time.After(ackResultWindow):
			}
			err = d.ackResult()
		} else {
			// The same upstream error is raised when the dialog was
			// terminated while the ACK was still in flight.
			err = ackWaitFailure(d.sess.LoadState(), err)
			if !errors.Is(err, ErrDialogEnded) {
				err = fmt.Errorf("%w: %w", ErrAckTimeout, err)
			}
		}
	}

	if err == nil {
		select {
		case <-d.ackChannel():
			// The ACK arrived; surface whatever readAck observed (media
			// finalization happens there).
			err = d.ackResult()
		default:
			// sipgo reports a 64*T1 timeout without an ACK as success; make
			// the missing ACK observable to WaitAck callers.
			err = fmt.Errorf("%w: %w", ErrAckTimeout, errNoAckReceived)
		}
	}

	if err != nil {
		d.setAckError(err)
	}
	d.notifyAckDone()
}

// errNoAckReceived mirrors sipgo's 64*T1 no-ACK outcome for the ErrAckTimeout
// message; the upstream error text itself is matched by isAckWaitRaceError.
var errNoAckReceived = errors.New("no ACK received")

// WaitAck blocks until the outcome of the dialog's 2xx ACK is known: the ACK
// was delivered and processed (returning the readAck error, usually nil),
// the ACK never arrived (ErrAckTimeout), or the dialog ended first
// (ErrDialogEnded). ctx cancels the wait and is returned as ctx.Err().
//
// It returns ErrDialogNotAnswered when no final 2xx was sent for the dialog.
// Answer/AnswerLate/RespondSDP return before the ACK is processed, so call
// WaitAck when the call must be fully established (ACK processed, media
// finalized) before proceeding.
func (d *InboundCall) WaitAck(ctx context.Context) error {
	if d.sess.LoadState() < sip.DialogStateEstablished {
		return ErrDialogNotAnswered
	}
	_, ackDone := d.ackChannels()
	select {
	case <-ackDone:
		return d.ackResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ackWaitFailure maps a 2xx ACK-wait failure to ErrDialogEnded when the wait
// ended because the dialog itself terminated instead of an ACK timeout.
// sipgo reports both with the same "No ACK received" string; the dialog state
// distinguishes them.
func ackWaitFailure(state sip.DialogState, err error) error {
	if state >= sip.DialogStateEnded {
		return errors.Join(ErrDialogEnded, err)
	}
	return err
}

// ackGraceWindow bounds how long respondSignal waits for diago's per-dialog
// ACK channel after sipgo's WaitForAck loop reported "No ACK received".
// sipgo's select can take the 64*T1 timeout branch even though the ACK was
// already delivered to diago's OnAck path (which closes the channel right
// before calling readAck). The ACK is ground truth, so this small window
// turns that race into success.
const ackGraceWindow = 100 * time.Millisecond

// ackResultWindow bounds how long respondSignal waits for readAck to finish
// after the ACK-arrival channel already reported success (sipgo race path).
// readAck failures are detected before the dialog is confirmed, so they are
// almost always visible immediately; the window only covers the rare case
// where the sipgo wait loop raced ahead of readAck.
const ackResultWindow = time.Second

// waitAckOrGrace reports whether the ACK was observed before the grace window
// elapsed or ctx was canceled. An already-closed channel wins immediately: a
// ctx cancellation that races with the ACK must not turn a delivered ACK into
// a failure.
func waitAckOrGrace(ctx context.Context, ackCh <-chan struct{}, grace time.Duration) bool {
	select {
	case <-ackCh:
		return true
	default:
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-ackCh:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		select {
		case <-ackCh:
			return true
		default:
			return false
		}
	}
}

// isAckWaitRaceError reports whether err matches the known sipgo v1.4.3
// "No ACK received" message raised from InboundCall.WriteResponse.
// We deliberately do NOT swallow every other error — transport errors,
// parser errors, and caller-side misuse must surface unchanged.
func isAckWaitRaceError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "No ACK received")
}

// byeSignal sends BYE applying signaling options (extra headers, request mutator).
func (d *InboundCall) byeSignal(ctx context.Context, params *signalParams) error {
	cont := d.sess.InviteRequest.Contact()
	bye := sip.NewRequest(sip.BYE, cont.Address)
	bye.SetTransport(d.sess.InviteRequest.Transport())

	if err := applyRequestSignal(bye, params); err != nil {
		return err
	}
	return d.sess.WriteBye(ctx, bye)
}

// Trying sends 100 Trying.
// Scope: ResponseOptions.
func (d *InboundCall) Trying(ctx context.Context, opts ...ResponseOptions) error {
	params, err := responseSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusTrying, "Trying", nil, params)
}

// Progress sends 183 Session Progress without SDP. It does not negotiate
// early media; use ProgressMedia when the caller must receive an SDP answer
// and early media in the 183.
// Scope: ResponseOptions.
func (d *InboundCall) Progress(ctx context.Context, opts ...ResponseOptions) error {
	params, err := responseSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusSessionInProgress, "Session Progress", nil, params)
}

// ProgressMedia sends 183 Session Progress and creates early media.
//
// Scope: ProgressMediaOptions.
func (d *InboundCall) ProgressMedia(ctx context.Context, opts ...ProgressMediaOptions) error {
	params, err := progressMediaSignalParams(opts...)
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
	if err := d.respondSignal(ctx, sip.StatusSessionInProgress, "Session Progress", body, params); err != nil {
		return err
	}
	return rtpSess.MonitorBackground()
}

// setupProgressMedia initializes (early) media session for 183 response.
func (d *InboundCall) setupProgressMedia(params *signalParams) (*media.RTPSession, error) {
	sess := params.Media.MediaSession
	if sess == nil {
		conf := signalMediaConfig(d.mediaConf, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return nil, err
		}
		sess = d.mediaSession
	} else {
		// Precreated sessions skip initMediaSessionFromConf; honor the
		// dialog-level hold music explicitly.
		d.setMohTone(signalMediaConfig(d.mediaConf, params).MusicOnHold)
	}
	rtpSess := media.NewRTPSession(sess)
	if err := d.setupRTPSession(rtpSess); err != nil {
		return nil, err
	}
	return rtpSess, nil
}

// Ringing sends 180 Ringing.
// Scope: ResponseOptions.
func (d *InboundCall) Ringing(ctx context.Context, opts ...ResponseOptions) error {
	params, err := responseSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusRinging, "Ringing", nil, params)
}

// waitConfirmed waits until the dialog is confirmed (the 2xx ACK was
// processed) when the dialog is already Established. Dialogs without a final
// 2xx fail immediately with ErrDialogNotAnswered; dialogs that end while
// waiting fail with ErrDialogEnded. It lets operations that must run on a
// confirmed dialog (ReInvite, Hold, Unhold, Refer) follow a non-blocking
// Answer without racing the ACK.
func (d *InboundCall) waitConfirmed(ctx context.Context) error {
	switch st := d.sess.LoadState(); {
	case st == sip.DialogStateConfirmed:
		return nil
	case st == sip.DialogStateEnded:
		return ErrDialogEnded
	case st < sip.DialogStateEstablished:
		return ErrDialogNotAnswered
	}

	stateCh := d.SIP().StateRead()
	for {
		select {
		case st := <-stateCh:
			switch st {
			case sip.DialogStateEnded:
				return ErrDialogEnded
			case sip.DialogStateConfirmed:
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SIP exposes the underlying sipgo dialog as the escape hatch.
func (d *InboundCall) SIP() *Dialog {
	return &d.sess.Dialog
}

// RemoteContact returns the current remote target of the dialog.
func (d *InboundCall) RemoteContact() *ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.remoteContactTarget != nil {
		return d.remoteContactTarget
	}
	return d.sess.InviteRequest.Contact()
}

// RespondSDP responds with 200 OK and provided SDP body.
// Options can customize status headers and Contact of the response.
// Scope: ResponseOptions (the body is a separate argument).
//
// Like Answer, it returns once the 200 OK was handed to the transaction
// layer; 2xx retransmission and the ACK wait continue in the background. Use
// WaitAck when the call must be fully established before proceeding.
func (d *InboundCall) RespondSDP(ctx context.Context, body []byte, opts ...ResponseOptions) error {
	params, err := responseSignalParams(opts...)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusOK, "OK", body, params)
}

// Answer creates media session and answers
// After this new AudioReader and AudioWriter are created for audio manipulation
// Options allow customizing Contact, headers, SDP body and media of the 200 OK response.
// Scope: AnswerOptions. Media overrides only apply when Answer creates the
// media session itself (no early media from ProgressMedia).
//
// Answer returns once the 200 OK was handed to the transaction layer; 2xx
// retransmission until the peer's ACK (RFC 3261 §13.3.1.4) continues in the
// background. Use WaitAck when the call must be fully established (ACK
// processed, media finalized) before proceeding. Calling Answer twice fails
// with ErrAlreadyAnswered.
// applyDialogCallbacks records the per-dialog callbacks carried by
// SignalOptions. Callers must hold d.mu.
func (d *InboundCall) applyDialogCallbacks(params *signalParams) {
	if params == nil {
		return
	}
	if params.Dialog.OnRefer != nil {
		d.onReferDialog = params.Dialog.OnRefer
	}
	if params.Dialog.OnMediaUpdate != nil {
		d.onMediaUpdate = params.Dialog.OnMediaUpdate
	}
}

// Answer creates the media session (unless ProgressMedia created it) and
// answers the call with 200 OK. Options allow customizing Contact, headers,
// SDP body and media of the response.
// Scope: AnswerOptions. Media overrides only apply when Answer creates the
// media session itself (no early media from ProgressMedia).
//
// Answer returns once the 200 OK was handed to the transaction layer; 2xx
// retransmission until the peer's ACK (RFC 3261 §13.3.1.4) continues in the
// background. Use WaitAck when the call must be fully established (ACK
// processed, media finalized) before proceeding. Calling Answer twice fails
// with ErrAlreadyAnswered.
func (d *InboundCall) Answer(ctx context.Context, opts ...AnswerOptions) error {
	policy := answerACKPolicy(opts...)
	params, err := answerSignalParams(opts...)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.applyDialogCallbacks(params)
	sess := d.mediaSession
	d.mu.Unlock()

	// Media Exists as early
	if sess != nil {
		// This will now block until ACK received with 64*T1 as max.
		body := sess.LocalSDP()
		if params != nil && params.Msg.Body != nil {
			body = params.Msg.Body
		}
		if err := d.respondSignal(ctx, sip.StatusOK, "OK", body, params); err != nil {
			return err
		}

		// Early media session was created by ProgressMedia and never finalized.
		// Finalize runs deferred media setup like DTLS handshake, same as answerSession.
		// MonitorBackground is already started by ProgressMedia and must not be started twice.
		if err := sess.Finalize(); err != nil {
			return err
		}
		if policy == ACKPolicyWait {
			return d.WaitAck(ctx)
		}
		return nil
	}

	rtpSess, err := d.newAnswerRTPSession(params)
	if err != nil {
		return err
	}
	if err := d.answerSession(ctx, rtpSess, params); err != nil {
		return err
	}
	if policy == ACKPolicyWait {
		return d.WaitAck(ctx)
	}
	return nil
}

// newAnswerRTPSession creates RTP session for answering. It honors custom
// media session passed with options, otherwise builds one from media config.
func (d *InboundCall) newAnswerRTPSession(params *signalParams) (*media.RTPSession, error) {
	sess := params.Media.MediaSession
	if sess == nil {
		conf := signalMediaConfig(d.mediaConf, params)
		if err := d.initMediaSessionFromConf(conf); err != nil {
			return nil, err
		}
		sess = d.mediaSession
	} else {
		// Precreated sessions skip initMediaSessionFromConf; honor the
		// dialog-level hold music explicitly.
		d.setMohTone(signalMediaConfig(d.mediaConf, params).MusicOnHold)
	}
	return media.NewRTPSession(sess), nil
}

// answerSession. It allows answering with custom RTP Session.
// NOTE: Not final API
func (d *InboundCall) answerSession(ctx context.Context, rtpSess *media.RTPSession, params *signalParams) error {
	// TODO: Use setupRTPSession
	sess := rtpSess.Sess
	sdp := d.sess.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in invite: %w", ErrSDPMissing)
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	d.mu.Lock()
	ierr := d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()
	if ierr != nil {
		return errors.Join(ierr, rtpSess.Close())
	}

	body := sess.LocalSDP()
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}

	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSignal(ctx, sip.StatusOK, "OK", body, params); err != nil {
		return err
	}

	if err := sess.Finalize(); err != nil {
		return err
	}
	// fmt.Println("--------SErver finalized")

	// Must be called after media and reader writer is setup. MonitorBackground
	// can fail with "rtp session is closed" if a sibling server handler (most
	// commonly OnBye) closes the underlying media while Answer is still
	// returning. Treat that race as benign: the dialog is tearing down anyway
	// and the caller will be unblocked by the surrounding close path.
	if err := rtpSess.MonitorBackground(); err != nil {
		if errors.Is(err, media.ErrRTPSessionClosed) {
			return nil
		}
		return err
	}
	return nil
}

func (d *InboundCall) setupRTPSession(rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess
	sdp := d.sess.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in invite: %w", ErrSDPMissing)
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	d.mu.Lock()
	err := d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()
	if err != nil {
		return errors.Join(err, rtpSess.Close())
	}
	return nil
}

// AnswerLate does answer with Late offer.
// Options allow customizing Contact, headers, SDP body and media of the 200 OK response.
// Scope: AnswerOptions.
//
// Like Answer, it returns once the 200 OK was handed to the transaction layer
// by default. The ACK of a late offer carries the remote answer; set
// AnswerOptions.ACKPolicy to ACKPolicyWait (or call WaitAck) to wait for the
// media to be fully negotiated before reading or writing media.
func (d *InboundCall) AnswerLate(ctx context.Context, opts ...AnswerOptions) error {
	policy := answerACKPolicy(opts...)
	params, err := answerSignalParams(opts...)
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.applyDialogCallbacks(params)
	d.mu.Unlock()

	rtpSess, err := d.newAnswerRTPSession(params)
	if err != nil {
		return err
	}
	sess := rtpSess.Sess
	localSDP := sess.LocalSDP()

	d.mu.Lock()
	err = d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()
	if err != nil {
		return errors.Join(err, rtpSess.Close())
	}

	body := localSDP
	if params != nil && params.Msg.Body != nil {
		body = params.Msg.Body
	}
	// The 200 OK is handed to the transaction layer; the ACK wait continues
	// in the background (WaitAck to observe it).
	if err := d.respondSignal(ctx, sip.StatusOK, "OK", body, params); err != nil {
		return err
	}
	if d.sess.InviteRequest.Body() == nil {
		// Late offer: the ACK carries the remote answer, so readAck applies
		// the remote SDP and starts the monitor when it arrives.
		if policy == ACKPolicyWait {
			return d.WaitAck(ctx)
		}
		return nil
	}
	// Must be called after media and reader writer is setup
	if err := rtpSess.MonitorBackground(); err != nil {
		return err
	}
	if policy == ACKPolicyWait {
		return d.WaitAck(ctx)
	}
	return nil
}

// readAck processes the ACK that confirms the dialog. It is unexported: ACKs
// are dispatched by diago's OnAck handler, callers must not drive it.
func (d *InboundCall) readAck(req *Request, tx ServerTransaction) error {
	if !d.matchesInviteAck(req) {
		// Only the ACK for our initial INVITE confirms the offer/answer and
		// the dialog. ACKs for peer re-INVITEs are handled by their own
		// exchange (handleMediaUpdate) and must not run — or fail — the
		// late-offer media processing here.
		return nil
	}
	if !d.ackProcessed.CompareAndSwap(false, true) {
		// Retransmitted ACK: the dialog is already confirmed and the media
		// answer applied.
		return nil
	}
	defer d.notifyAckDone()

	// A late-offer INVITE (no SDP) makes the ACK answer mandatory: the dialog
	// has no remote media address otherwise (RFC 3261 §13.3.1.4).
	lateOffer := d.sess.InviteRequest != nil && d.sess.InviteRequest.Body() == nil

	// Apply the late-offer SDP under the lock, resolving the media session
	// for Finalize which must run outside it: Finalize performs the DTLS
	// handshake, a network operation that would block Answer/Close and SIP
	// handling on d.mu for its duration.
	sess, err := func() (*media.MediaSession, error) {
		d.mu.Lock()
		defer d.mu.Unlock()

		if d.mediaSession == nil {
			return nil, nil
		}
		contentType := req.ContentType()
		body := req.Body()
		if contentType == nil || body == nil || contentType.Value() != "application/sdp" {
			if lateOffer {
				return nil, fmt.Errorf("late offer ACK without sdp answer: %w", ErrSDPMissing)
			}
			return nil, nil
		}
		// This is Late offer response
		return d.applyLateOfferAnswerUnsafe(body)
	}()
	if err == nil && sess != nil {
		// Finalize session. A failed finalize (ex. DTLS handshake) must
		// surface: proceeding with dead media is worse than hanging up.
		err = sess.Finalize()
	}
	if err == nil && lateOffer {
		// AnswerLate cannot start the monitor before the ACK: the remote SDP
		// arrives here, so start it now that RemoteSDP was applied.
		err = d.startMonitorForAnswer()
	}
	if err != nil {
		// The peer acknowledged the 2xx, so the dialog is confirmed from its
		// point of view: advance sipgo state first (close would otherwise
		// try to decline an already-answered INVITE, which cannot be put on
		// the wire) and then send a real BYE. Record the failure before the
		// state change so a waiting respondSignal surfaces it.
		d.setAckError(err)
		aerr := d.sess.ReadAck(req, tx)
		herr := d.hangup(d.Context())
		return errors.Join(err, aerr, herr)
	}

	aerr := d.sess.ReadAck(req, tx)
	if aerr != nil {
		d.setAckError(aerr)
	}
	return aerr
}

// startMonitorForAnswer starts RTCP/media monitoring on the dialog's current
// RTP session. It is used by the late-offer ACK path, which is the earliest
// point where the remote address is known.
func (d *InboundCall) startMonitorForAnswer() error {
	d.mu.Lock()
	rtpSess := d.rtpSession
	d.mu.Unlock()
	if rtpSess == nil {
		return nil
	}
	return rtpSess.MonitorBackground()
}

// hangup terminates dialog. When a final 2xx response was sent (dialog
// Established or Confirmed) BYE is sent — sipgo waits for the ACK first, per
// RFC 3261 §15. Otherwise the INVITE is declined with 480 (and nil is
// returned — declining succeeded).
// Options allow customizing headers (ex. Reason), Contact and body of the
// outgoing message.
// Scope: CloseOptions.
//
// The BYE waits for the peer's 200 and honors ctx. The 480 decline of an
// unconfirmed INVITE is sent even when ctx is already canceled: the caller
// asked to terminate and the peer is still waiting for a final response, so
// skipping the decline would hang the INVITE transaction.
func (d *InboundCall) hangup(ctx context.Context, opts ...CloseOptions) error {
	params, err := closeSignalParams(opts...)
	if err != nil {
		return err
	}

	state := d.sess.LoadState()
	if state >= sip.DialogStateEstablished && state != sip.DialogStateEnded {
		return d.byeSignal(ctx, params)
	}
	// The decline must go out even when ctx is already canceled (see the
	// method doc); context.WithoutCancel keeps the values but drops the
	// cancellation. The send itself is bounded by the INVITE transaction.
	return d.respondSignal(context.WithoutCancel(ctx), sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil, params)
}

// ReInvite sends a re-INVITE with the current media session.
// Scope: RequestOptions; media options
// are rejected.
// The exchange (491 retry, 2xx ACK) is driven by reInviteExchange, shared
// with the client dialog.
func (d *InboundCall) ReInvite(ctx context.Context, opts ...RequestOptions) error {
	params, err := requestSignalParams(opts...)
	if err != nil {
		return err
	}
	if err := d.waitConfirmed(ctx); err != nil {
		return fmt.Errorf("reinvite: %w", err)
	}

	d.mu.Lock()
	if d.mediaSession == nil {
		d.mu.Unlock()
		return ErrDialogNotAnswered
	}
	sdpBody := d.mediaSession.LocalSDP()
	contact := d.remoteContactUnsafe()
	var localContact *ContactHeader
	if d.sess.InviteResponse != nil {
		localContact = d.sess.InviteResponse.Contact()
	}
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	if err := buildReInviteRequest(req, sdpBody, localContact, params); err != nil {
		return err
	}

	_, err = reInviteExchange(ctx, d, req)
	return err
}

// reInviteMediaSession updates with full new media session
// media MUST BE Forked
func (d *InboundCall) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *signalParams) error {
	if err := d.waitConfirmed(ctx); err != nil {
		return err
	}
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

// writeReInviteACK sends the final ACK of a re-INVITE exchange without
// options; see reInviteExchange.
func (d *InboundCall) writeReInviteACK(ctx context.Context, target URI) error {
	return d.ack(ctx, target, nil)
}

func (d *InboundCall) ack(ctx context.Context, remoteTarget URI, body []byte) error {
	// inviteRequest := d.sess.InviteRequest
	// recipient := &inviteRequest.Recipient
	// if contact := d.sess.InviteResponse.Contact(); contact != nil {
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

	if err := d.sess.WriteRequest(ackRequest); err != nil {
		return err
	}

	// if err := d.sess.WriteAck(ctx, ackRequest); err != nil {
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

func (d *InboundCall) remoteContactUnsafe() *ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	return d.sess.InviteRequest.Contact()
}

// Refer tries todo refer (blind transfer) on call.
// Options allow customizing headers of the REFER request and receiving the
// transfer status via ReferOptions.OnReferNotify.
// Scope: ReferOptions.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *InboundCall) Refer(ctx context.Context, referTo URI, opts ...ReferOptions) error {
	params, err := referSignalParams(opts...)
	if err != nil {
		return err
	}
	// Wait for the ACK of our 2xx (non-blocking Answer) before dereferencing
	// the remote Contact. Unanswered dialogs fail with ErrDialogNotAnswered.
	if err := d.waitConfirmed(ctx); err != nil {
		return fmt.Errorf("refer: %w", err)
	}
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if params.Dialog.OnReferNotify != nil {
		d.onReferNotify = params.Dialog.OnReferNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.sess.InviteResponse.Contact().Address, params)
}

func (d *InboundCall) handleReferNotify(req *Request, tx ServerTransaction) {
	dialogHandleReferNotify(d, req, tx)
}

func (d *InboundCall) handleRefer(dg *Agent, req *Request, tx ServerTransaction) {
	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	if err := dialogHandleRefer(d, dg, req, tx, onRefDialog); err != nil {
		dg.log.Error("Failed to handle REFER", "error", err)
	}
}

func (d *InboundCall) handleReInvite(req *Request, tx ServerTransaction) error {
	// Check is current pending dialog
	if state := d.sess.LoadState(); state == sip.DialogStateEstablished {
		// RFC 3261 §14.2 — UAS Behavior
		// If a UAS receives an INVITE request for an existing dialog while another INVITE transaction is in progress, it MUST return a 491 (Request Pending) response to the new INVITE.”
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
	}

	// NOTE: Calling ReadRequest increases remote CSEQ.
	// We should not call this until dialog is confirmed, otherwise any intermidiate response
	// will have wrong CSEQ
	if err := d.sess.ReadRequest(req, tx); err != nil {
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

	return d.handleMediaUpdate(req, tx, d.sess.InviteResponse.Contact())
}

func (d *InboundCall) readSIPInfoDTMF(req *Request, tx ServerTransaction) error {
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
func (d *InboundCall) Hold(ctx context.Context, opts ...HoldOptions) error {
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
func (d *InboundCall) Unhold(ctx context.Context, opts ...RequestOptions) error {
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
