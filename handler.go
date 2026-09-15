// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo/sip"
)

// DialogHandler handles one inbound dialog (call leg).
//
// Contract (docs/contracts.md §6):
//   - ctx is canceled when the serve context is canceled (Shutdown) or the
//     dialog terminates. Long-running work must honor it.
//   - Returning nil hands the call back to the framework, which hangs it up
//     (BYE once confirmed, otherwise a 480 decline) and releases the media.
//   - Returning an error terminates the call and maps the error to a final
//     SIP response: ErrDecline -> 480, ErrBusy -> 486, ErrNotFound -> 404,
//     *ResponseError -> custom, anything else -> 500. A confirmed call is
//     ended with BYE carrying a matching Reason header instead.
//   - Wrap ErrResponseSent when the handler already wrote its own final
//     response (for example DigestAuthServer.AuthorizeDialog); the framework
//     then only tears the dialog down.
//
// A handler that wants the call alive must block until the call ends (or hang
// up itself before returning); goroutines spawned in the handler must not rely
// on the dialog afterwards.
type DialogHandler func(ctx context.Context, d *DialogServerSession) error

// defaultDialogHandler is used when Serve is given a nil handler: it refuses
// calls instead of accepting and immediately hanging them up.
func defaultDialogHandler(ctx context.Context, d *DialogServerSession) error {
	return &ResponseError{Code: sip.StatusServiceUnavailable, Reason: "Service Unavailable"}
}

// handlerErrorResponse maps a DialogHandler error to a final SIP response.
func handlerErrorResponse(err error) (code int, reason string, headers []sip.Header) {
	var resErr *ResponseError
	if errors.As(err, &resErr) {
		code, reason, headers = resErr.Code, resErr.Reason, resErr.Headers
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
	default:
		return "Error"
	}
}

// respondHandlerError terminates the dialog according to a DialogHandler
// error. It returns nil when no response is required: the handler already
// responded, or the call is being torn down (serve/dialog context done).
func (d *DialogServerSession) respondHandlerError(ctx context.Context, herr error) error {
	if errors.Is(herr, ErrResponseSent) {
		// The handler wrote its own final response (ex. the 401 challenge);
		// the framework only cleans up.
		return nil
	}
	if errors.Is(herr, context.Canceled) || errors.Is(herr, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			// The call is being torn down (Shutdown / peer BYE / teardown
			// timeout); a response cannot be delivered anymore.
			return nil
		}
		// Handler's own context error while the dialog is still alive: fall
		// through so the INVITE is answered 500 instead of being left
		// unanswered until transaction timeout.
	}

	code, reason, headers := handlerErrorResponse(herr)
	if d.LoadState() >= sip.DialogStateConfirmed {
		// The call was answered; a re-response is no longer possible.
		opts := []SignalOption{WithHeader("Reason", dialogReasonHeader(code, reason))}
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
// (Shutdown) or the dialog terminates.
func (dg *Diago) dialogHandlerContext(dialogCtx context.Context) (context.Context, context.CancelFunc) {
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
