// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo/sip"
)

// CallHandler handles one inbound dialog (call leg).
//
// Contract:
//   - ctx is canceled when the serve context is canceled (Close), or the
//     dialog terminates. Long-running work must honor it.
//   - The handler decides how the call is answered. Returning before a final
//     response was sent hands the call back to the framework, which declines
//     the still-pending INVITE (480, or the error mapping below).
//   - Answering (Answer/AnswerLate/RespondSDP) and then returning keeps the
//     call alive: the framework owns it until the peer ends it, Close runs
//     or the handler hangs it up. A handler that wants to control the call
//     beyond its return can keep using the dialog (or spawn goroutines) until
//     ctx is canceled / the dialog ends.
//   - Returning an error terminates the call: before a final response is sent
//     the error is mapped to a final SIP response — ErrDecline -> 603,
//     ErrTemporarilyUnavailable -> 480, ErrBusy -> 486, ErrNotFound -> 404,
//     *ResponseError -> custom, anything else -> 500. An answered call is
//     ended with BYE carrying a matching Reason header instead.
//   - Wrap ErrResponseSent when the handler already wrote its own final
//     response (for example DigestAuthServer.AuthorizeDialog); the framework
//     then only tears the dialog down.
type CallHandler func(ctx context.Context, d *InboundCall) error

// defaultCallHandler is used when Serve is given a nil handler: it refuses
// calls instead of accepting and immediately hanging them up.
func defaultCallHandler(ctx context.Context, d *InboundCall) error {
	return newResponseError(sip.StatusServiceUnavailable, "Service Unavailable")
}

// handlerErrorResponse maps a CallHandler error to a final SIP response.
func handlerErrorResponse(err error) (code int, reason string, headers []Header) {
	var resErr *ResponseError
	if errors.As(err, &resErr) {
		code, reason, headers = resErr.Code(), resErr.Reason(), resErr.Headers()
		if code < 300 {
			// A handler error is never a success: keep the dialog state
			// consistent with a 5xx rather than emitting a bogus 2xx.
			code, reason = sip.StatusInternalServerError, "Server Internal Error"
		}
		if reason == "" {
			reason = defaultReasonPhrase(code)
		}
		return code, reason, headers
	}

	switch {
	case errors.Is(err, ErrDecline):
		return sip.StatusGlobalDecline, "Decline", nil
	case errors.Is(err, ErrTemporarilyUnavailable):
		return sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil
	case errors.Is(err, ErrBusy):
		return sip.StatusBusyHere, "Busy Here", nil
	case errors.Is(err, ErrNotFound):
		return sip.StatusNotFound, "Not Found", nil
	default:
		return sip.StatusInternalServerError, "Server Internal Error", nil
	}
}

func defaultReasonPhrase(code int) string {
	switch code {
	case sip.StatusNotFound:
		return "Not Found"
	case sip.StatusTemporarilyUnavailable:
		return "Temporarily Unavailable"
	case sip.StatusBusyHere:
		return "Busy Here"
	case sip.StatusInternalServerError:
		return "Server Internal Error"
	case sip.StatusServiceUnavailable:
		return "Service Unavailable"
	case sip.StatusGlobalDecline:
		return "Decline"
	default:
		return "Error"
	}
}

// respondHandlerError terminates the dialog according to a CallHandler
// error. It returns nil when no response is required: the handler already
// responded, or the call is being torn down (serve/dialog context done).
func (d *InboundCall) respondHandlerError(ctx context.Context, herr error) error {
	if errors.Is(herr, ErrResponseSent) {
		// The handler wrote its own final response (ex. the 401 challenge);
		// the framework only cleans up.
		return nil
	}
	if errors.Is(herr, context.Canceled) || errors.Is(herr, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			// The call is being torn down (Close / peer BYE / teardown
			// timeout); a response cannot be delivered anymore.
			return nil
		}
		// Handler's own context error while the dialog is still alive: fall
		// through so the INVITE is answered 500 instead of being left
		// unanswered until transaction timeout.
	}

	code, reason, headers := handlerErrorResponse(herr)
	if d.sess.LoadState() >= sip.DialogStateEstablished && d.sess.LoadState() != sip.DialogStateEnded {
		// The 2xx was already sent (possibly still awaiting the ACK): a
		// re-response is no longer possible, so terminate with BYE.
		opts := []HangupOption{WithHeaders(sip.NewHeader("Reason", dialogReasonHeader(code, reason)))}
		for _, h := range headers {
			if h != nil {
				opts = append(opts, WithHeaders(h))
			}
		}
		return d.Hangup(ctx, opts...)
	}

	params := &signalParams{
		Msg: signalMsgParams{Headers: headers},
	}
	return d.respondSignal(ctx, code, reason, nil, params)
}

// dialogReasonHeader builds an RFC 3326 Reason value for a BYE caused by a
// handler error.
func dialogReasonHeader(code int, reason string) string {
	return fmt.Sprintf("SIP ;cause=%d ;text=%q", code, reason)
}

// dialogHandlerContext derives the handler context from the serve context and
// the dialog lifetime: it is canceled when either the serve is canceled
// (Close) or the dialog terminates.
func (dg *Agent) callHandlerContext(dialogCtx context.Context) (context.Context, context.CancelFunc) {
	dg.serveMu.Lock()
	serveCtx := dg.serveCtx
	dg.serveMu.Unlock()
	if serveCtx == nil {
		serveCtx = context.Background()
	}

	ctx, cancel := context.WithCancel(serveCtx)
	stop := context.AfterFunc(dialogCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
