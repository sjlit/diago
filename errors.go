// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"errors"
	"fmt"

	"github.com/emiago/sipgo/sip"
)

// Lifecycle sentinels for dialog media operations. All media entry points
// (audio readers/writers, playback factories, Listen*, Echo) return errors
// matching these instead of panicking or returning bare errors when the dialog
// is in the corresponding lifecycle state.
var (
	// ErrDialogNotAnswered is returned when an operation requires an answered
	// dialog with negotiated media, but no active media session exists yet
	// (dialog not answered) or no invite response was received.
	ErrDialogNotAnswered = errors.New("dialog session not answered")

	// ErrDialogClosed is returned by media operations after the dialog media
	// was closed locally with Close (or the dialog ended and the framework
	// closed it).
	ErrDialogClosed = errors.New("dialog media closed")

	// ErrDialogEnded is returned (joined with the underlying cause) when an
	// operation waiting on the dialog is aborted because the dialog
	// terminated first — for example a BYE processed while a 2xx response was
	// still waiting for its ACK. Callers can use errors.Is to distinguish
	// this from a genuine transport or ACK-wait failure.
	ErrDialogEnded = errors.New("dialog ended")

	// ErrSDPMissing is returned when an SDP body is required but absent:
	// an INVITE without an offer, a 2xx answering an offer without SDP, or a
	// delayed-offer flow missing the answer. Callers can use errors.Is to
	// distinguish "peer sent no SDP" from transport or parse failures.
	ErrSDPMissing = errors.New("sdp missing")

	// ErrMediaSessionMissing is returned when an operation requires an
	// active media (RTP) session but none has been installed yet. It is the
	// internal early-media probe result and is not fatal by itself.
	ErrMediaSessionMissing = errors.New("media session missing")

	// ErrRemoteContactMissing is returned by re-INVITE handling when a 2xx
	// carries no Contact and the dialog has no remote target to fall back to.
	ErrRemoteContactMissing = errors.New("remote contact missing")

	// ErrDTMFUnsupported is returned by SendDTMF when the peer did not
	// negotiate telephone-event and the selected method has no fallback.
	ErrDTMFUnsupported = errors.New("dtmf: telephone-event not negotiated")

	// ErrMusicOnHoldActive is returned by PlayMusicOnHold when a hold-music
	// loop is already running on the dialog (one loop per dialog).
	ErrMusicOnHoldActive = errors.New("music on hold already active")

	// ErrMusicOnHoldNoTone is returned by PlayMusicOnHold when neither the
	// WithMoHTone option nor the dialog MediaConfig provides a hold music tone.
	ErrMusicOnHoldNoTone = errors.New("music on hold: no tone configured")
)

// DialogHandler error mapping. Return (or wrap) one of these sentinels from a
// DialogHandler to select the final response the framework sends. For a call
// that is already answered the framework sends BYE with a matching Reason
// header (RFC 3326) instead of a response.
var (
	// ErrDecline maps to 480 Temporarily Unavailable.
	ErrDecline = errors.New("call declined")
	// ErrBusy maps to 486 Busy Here.
	ErrBusy = errors.New("call busy")
	// ErrNotFound maps to 404 Not Found.
	ErrNotFound = errors.New("not found")
	// ErrResponseSent tells the framework the handler already wrote its own
	// final response (for example DigestAuthServer.AuthorizeDialog sends the
	// 401 challenge). The framework then only tears the dialog down and never
	// writes a second response. Wrap it alongside the original error:
	// errors.Join(ErrResponseSent, err).
	ErrResponseSent = errors.New("response already sent")
)

// ResponseError lets a DialogHandler map an error to an arbitrary final SIP
// response. Code must be a final non-2xx status; values below 300 are treated
// as 500 to keep the dialog state consistent.
type ResponseError struct {
	Code    int
	Reason  string
	Headers []sip.Header
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("dialog handler response: %d %s", e.Code, e.Reason)
}
