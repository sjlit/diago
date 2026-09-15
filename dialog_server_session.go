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

type OnReferDialogFunc func(referDialog *DialogClientSession) error

// DialogServerSession represents inbound channel
type DialogServerSession struct {
	*sipgo.DialogServerSession

	// MediaSession *media.MediaSession
	dialogMedia

	onReferDialog OnReferDialogFunc

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
	ackMu sync.Mutex
	ackCh chan struct{}
}

// DialogMedia returns the media half of the session. It is never nil.
func (d *DialogServerSession) DialogMedia() *DialogMedia {
	return &d.dialogMedia
}

// ackChannel lazily allocates and returns the per-dialog ACK channel.
// It is closed by diago's OnAck handler when the matching ACK arrives.
func (d *DialogServerSession) ackChannel() <-chan struct{} {
	d.ackMu.Lock()
	defer d.ackMu.Unlock()
	if d.ackCh == nil {
		d.ackCh = make(chan struct{})
	}
	return d.ackCh
}

// notifyAck closes the dialog's ack channel, signaling any pending
// respondSignal waiters that the ACK has been delivered. It is safe to
// call multiple times (subsequent calls are no-ops).
func (d *DialogServerSession) notifyAck() {
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
func (d *DialogServerSession) matchesInviteAck(req *sip.Request) bool {
	if req == nil || d.InviteRequest == nil {
		return false
	}
	reqCSeq, inviteCSeq := req.CSeq(), d.InviteRequest.CSeq()
	if reqCSeq == nil || inviteCSeq == nil {
		return false
	}
	return reqCSeq.SeqNo == inviteCSeq.SeqNo
}

func (d *DialogServerSession) ID() string {
	return d.DialogServerSession.ID
}

// Close frees local resources (media stack and dialog cleanup hooks).
// It is idempotent and does NOT send any SIP message. Server dialogs are
// closed by the framework when the serve handler returns.
func (d *DialogServerSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.dialogMedia.Close()
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
//
// For final 2xx, this defends against a known sipgo v1.4.3 race: sipgo's
// WaitForAck loop can exit with "No ACK received" even though the ACK was
// delivered and processed by diago's OnAck/readAck path. We rely on a
// diago-managed ackCh closed by the OnAck handler as ground truth: if
// sipgo's loop races past the state notification, we treat the call as
// successful because the peer has acknowledged.
func (d *DialogServerSession) respondSignal(ctx context.Context, statusCode int, reason string, body []byte, params *signalParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

	// Only 2xx waits for ACK and is affected by the race.
	if !res.IsSuccess() {
		return d.DialogServerSession.WriteResponse(res)
	}

	ackCh := d.ackChannel()
	err := d.DialogServerSession.WriteResponse(res)
	if err == nil {
		return nil
	}

	// Race defense: if sipgo's WaitForAck loop exited with "No ACK
	// received" but diago's OnAck handler already closed ackCh, the peer
	// really did acknowledge — the failure is purely a sipgo internal
	// race, not a real transport problem.
	if isAckWaitRaceError(err) {
		if waitAckOrGrace(ctx, ackCh, ackGraceWindow) {
			return nil
		}
		// The same upstream error is raised when the dialog was terminated
		// while the ACK was still in flight (a BYE processed first wakes the
		// wait loop with DialogStateEnded). Map that case to a sentinel so
		// callers do not have to parse the sipgo message.
		return ackWaitFailure(d.LoadState(), err)
	}
	return err
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
// "No ACK received" message raised from DialogServerSession.WriteResponse.
// We deliberately do NOT swallow every other error — transport errors,
// parser errors, and caller-side misuse must surface unchanged.
func isAckWaitRaceError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "No ACK received")
}

// HangupAndClose terminates the dialog with Hangup (BYE when confirmed,
// otherwise a decline) and then releases local resources with Close. The
// local close always runs, so resources are not leaked even when Hangup
// fails.
func (d *DialogServerSession) HangupAndClose(ctx context.Context, opts ...SignalOption) error {
	return errors.Join(d.Hangup(ctx, opts...), d.Close())
}

// byeSignal sends BYE applying signaling options (extra headers, request mutator).
func (d *DialogServerSession) byeSignal(ctx context.Context, params *signalParams) error {
	cont := d.InviteRequest.Contact()
	bye := sip.NewRequest(sip.BYE, cont.Address)
	bye.SetTransport(d.InviteRequest.Transport())

	if err := applyRequestSignal(bye, params); err != nil {
		return err
	}
	return d.DialogServerSession.WriteBye(ctx, bye)
}

// Trying sends 100 Trying.
// Scope: WithHeaders, WithContact, WithResponseMutator.
func (d *DialogServerSession) Trying(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Trying", scopeTrying)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusTrying, "Trying", nil, params)
}

// Progress sends 183 Session Progress without SDP. It does not negotiate
// early media; use ProgressMedia when the caller must receive an SDP answer
// and early media in the 183.
// Scope: WithHeaders, WithContact, WithResponseMutator.
func (d *DialogServerSession) Progress(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Progress", scopeProgress)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusSessionInProgress, "Session Progress", nil, params)
}

// ProgressMedia sends 183 Session Progress and creates early media.
//
// Scope: WithHeaders, WithContact, WithBody, WithResponseMutator, plus the
// media overrides (WithCodecs, WithRTPNAT, WithMediaBindIP, WithMediaExternalIP,
// WithMediaDTLS, WithSDPSessionName, WithMediaSession, WithMusicOnHold).
func (d *DialogServerSession) ProgressMedia(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "ProgressMedia", scopeProgressMedia)
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
func (d *DialogServerSession) setupProgressMedia(params *signalParams) (*media.RTPSession, error) {
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
// Scope: WithHeaders, WithContact, WithResponseMutator.
func (d *DialogServerSession) Ringing(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Ringing", scopeRinging)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusRinging, "Ringing", nil, params)
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
// Scope: WithHeaders, WithContact, WithResponseMutator (use the body argument).
//
// The 2xx exchange retransmits the response until the peer's ACK arrives and
// waits for it before returning (RFC 3261 §13.3.1.4; bounded by 64*T1 ≈ 32s
// without an ACK). ctx is honored before the response is written but does not
// interrupt that ACK wait, so callers should not rely on ctx for a strict
// upper bound here.
func (d *DialogServerSession) RespondSDP(ctx context.Context, body []byte, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "RespondSDP", scopeRespondSDP)
	if err != nil {
		return err
	}
	return d.respondSignal(ctx, sip.StatusOK, "OK", body, params)
}

// Answer creates media session and answers
// After this new AudioReader and AudioWriter are created for audio manipulation
// Options allow customizing Contact, headers, SDP body and media of the 200 OK response.
// Scope: ProgressMedia options plus WithOnMediaUpdate and WithOnRefer; media
// overrides only apply when Answer creates the media session itself (no early
// media from ProgressMedia).
//
// The 2xx exchange retransmits the response until the peer's ACK arrives and
// waits for it before returning (RFC 3261 §13.3.1.4; bounded by 64*T1 ≈ 32s
// without an ACK). ctx is honored before the response is written but does not
// interrupt that ACK wait, so Answer can outlive its ctx on a slow/absent ACK.
func (d *DialogServerSession) Answer(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Answer", scopeAnswer)
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
		if err := d.respondSignal(ctx, sip.StatusOK, "OK", body, params); err != nil {
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
	return d.answerSession(ctx, rtpSess, params)
}

// newAnswerRTPSession creates RTP session for answering. It honors custom
// media session passed with options, otherwise builds one from media config.
func (d *DialogServerSession) newAnswerRTPSession(params *signalParams) (*media.RTPSession, error) {
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

// answerSession. It allows answering with custom RTP Session.
// NOTE: Not final API
func (d *DialogServerSession) answerSession(ctx context.Context, rtpSess *media.RTPSession, params *signalParams) error {
	// TODO: Use setupRTPSession
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
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

func (d *DialogServerSession) setupRTPSession(rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
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
// Scope: WithHeaders, WithContact, WithBody, WithResponseMutator, plus the
// media overrides (WithCodecs, WithRTPNAT, WithMediaBindIP, WithMediaExternalIP,
// WithMediaDTLS, WithSDPSessionName, WithMediaSession, WithMusicOnHold).
//
// Like Answer, the 2xx ACK wait (up to 64*T1 ≈ 32s) is not interruptible by
// ctx.
func (d *DialogServerSession) AnswerLate(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "AnswerLate", scopeAnswerLate)
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
	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSignal(ctx, sip.StatusOK, "OK", body, params); err != nil {
		return err
	}
	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

// readAck processes the ACK that confirms the dialog. It is unexported: ACKs
// are dispatched by diago's OnAck handler, callers must not drive it.
func (d *DialogServerSession) readAck(req *sip.Request, tx sip.ServerTransaction) error {
	// Apply the late-offer SDP under the lock, resolving the media session
	// for Finalize which must run outside it: Finalize performs the DTLS
	// handshake, a network operation that would block Answer/Hangup and SIP
	// handling on d.mu for its duration. Ordering is preserved - Finalize
	// completes before the dialog is confirmed below.
	sess, err := func() (*media.MediaSession, error) {
		d.mu.Lock()
		defer d.mu.Unlock()

		if d.mediaSession == nil {
			return nil, nil
		}
		contentType := req.ContentType()
		if contentType == nil {
			return nil, nil
		}
		body := req.Body()
		if body == nil || contentType.Value() != "application/sdp" {
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
	if err != nil {
		e := d.Hangup(d.Context())
		return errors.Join(err, e)
	}

	return d.DialogServerSession.ReadAck(req, tx)
}

// Hangup terminates dialog. When dialog is confirmed BYE is sent,
// otherwise the INVITE is declined with 480 (and nil is returned — declining
// succeeded). Options allow customizing headers (ex. Reason), Contact and body
// of the outgoing message.
// Scope: WithHeaders, WithContact, WithRequestMutator (BYE),
// WithResponseMutator (480 decline).
func (d *DialogServerSession) Hangup(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Hangup", scopeServerHangup)
	if err != nil {
		return err
	}

	state := d.LoadState()
	if state >= sip.DialogStateConfirmed {
		return d.byeSignal(ctx, params)
	}
	return d.respondSignal(ctx, sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil, params)
}

// ReInvite sends a re-INVITE with the current media session.
// Scope: WithHeaders, WithContact, WithBody, WithRequestMutator; media options
// are rejected.
// The exchange (491 retry, 2xx ACK) is driven by reInviteExchange, shared
// with the client dialog.
func (d *DialogServerSession) ReInvite(ctx context.Context, opts ...SignalOption) error {
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
	var localContact *sip.ContactHeader
	if d.InviteResponse != nil {
		localContact = d.InviteResponse.Contact()
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
func (d *DialogServerSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession, params *signalParams) error {
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
func (d *DialogServerSession) writeReInviteACK(ctx context.Context, target sip.Uri) error {
	return d.ack(ctx, target, nil)
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

// Refer tries todo refer (blind transfer) on call.
// Options allow customizing headers of the REFER request and receiving the
// transfer status via WithOnReferNotify.
// Scope: WithHeaders, WithContact, WithRequestMutator, WithOnReferNotify.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogServerSession) Refer(ctx context.Context, referTo sip.Uri, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Refer", scopeRefer)
	if err != nil {
		return err
	}
	// Guard before dereferencing InviteResponse: it is nil until the first
	// response is written (dialogRefer's own check would run too late).
	if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
		return fmt.Errorf("can only be called on answered dialog")
	}
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if params.Dialog.OnReferNotify != nil {
		d.onReferNotify = params.Dialog.OnReferNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, params)
}

func (d *DialogServerSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
	dialogHandleReferNotify(d, req, tx)
}

func (d *DialogServerSession) handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
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
func (d *DialogServerSession) Hold(ctx context.Context, opts ...SignalOption) error {
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
func (d *DialogServerSession) Unhold(ctx context.Context, opts ...SignalOption) error {
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
