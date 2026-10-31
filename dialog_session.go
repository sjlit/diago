// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Call is the common contract of inbound and outbound dialogs
// (*InboundCall and *OutboundCall). Cross-cutting APIs (Bridge, DialBridge,
// FindCall, WithOriginator) accept it; type-assert to the concrete direction
// when direction-specific methods are needed.
type Call interface {
	ID() string
	Context() context.Context
	State() CallState
	// FromUser returns the user part of the From header (the caller).
	FromUser() string
	// ToUser returns the user part of the To header (the callee).
	ToUser() string
	// From returns the full From URI (the caller).
	From() URI
	// To returns the full To URI (the callee).
	To() URI
	// Transport returns the transport of the dialog's INVITE (ex. "UDP").
	Transport() string
	// Hold puts the dialog on hold (media sendonly).
	Hold(ctx context.Context, opts ...HoldOptions) error
	// Unhold takes the dialog back from hold (media sendrecv).
	Unhold(ctx context.Context, opts ...RequestOptions) error
	// ReInvite sends a re-INVITE with the current media session.
	ReInvite(ctx context.Context, opts ...RequestOptions) error
	// Refer sends a blind transfer REFER.
	Refer(ctx context.Context, referTo URI, opts ...ReferOptions) error
	// Media returns the media half of the dialog. It is never nil and
	// stays valid for the dialog's lifetime.
	Media() *Media
	// Close terminates the dialog (CANCEL/BYE/decline as appropriate) and
	// releases local resources. It is idempotent.
	Close(ctx context.Context, opts ...CloseOptions) error
}

// RawCall exposes the underlying sipgo dialog. It is the explicit escape
// hatch for features diago does not wrap; core call operations do not depend
// on it.
type RawCall interface {
	SIP() *Dialog
	Do(ctx context.Context, req *Request) (*Response, error)
}

var (
	_ Call = (*OutboundCall)(nil)
	_ Call = (*InboundCall)(nil)
)

//
// Here are many common functions built for dialog
//

func dialogRefer(ctx context.Context, d Call, recipient URI, referTo URI, refferedBy URI, params *signalParams) error {
	raw, ok := d.(RawCall)
	if !ok {
		return fmt.Errorf("refer: %w", ErrRawCallUnsupported)
	}
	if d.State() != CallStateConfirmed {
		return fmt.Errorf("refer: %w", ErrDialogNotAnswered)
	}

	req := sip.NewRequest(sip.REFER, recipient)
	// Invite request tags must be preserved but switched
	req.AppendHeader(sip.NewHeader("Refer-To", uri2Header(referTo)))
	req.AppendHeader(sip.NewHeader("Referred-By", uri2Header(refferedBy)))
	if params != nil {
		if params.Msg.Contact != nil {
			setSignalContact(req, params.Msg.Contact)
		}
		for _, h := range params.Msg.Headers {
			if h == nil {
				continue
			}

			switch h.Name() {
			// Avoid duplicates
			case "Refer-To", "refer-to", "Referred-By", "reffered-by":
				req.ReplaceHeader(h)
				continue
			default:
				req.AppendHeader(h)
			}
		}
		if params.Msg.MutateRequest != nil {
			if err := params.Msg.MutateRequest(req); err != nil {
				return err
			}
		}
	}

	res, err := raw.Do(ctx, req)
	if err != nil {
		return err
	}

	if res.StatusCode != sip.StatusAccepted {
		return newDialogResponseError(res)
	}
	return nil
}

func dialogHandleReferNotify(d Call, req *Request, tx ServerTransaction) {
	// TODO how to know this is refer
	contentTypeHeader := req.ContentType()
	if contentTypeHeader == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}
	contentType := contentTypeHeader.Value()
	// For now very basic check
	if !strings.HasPrefix(contentType, "message/sipfrag") {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	frag := string(req.Body())
	if len(frag) < len("SIP/2.0 100 xx") {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))

	// TODO: We need find better way to store this on refer.
	// best case this would be dialogSIP
	med := d.Media()
	med.mu.Lock()
	onNot := med.onReferNotify
	med.mu.Unlock()

	code := func() int {
		v := strings.TrimPrefix(frag[:11], "SIP/2.0 ")
		switch v {
		case "100":
			return sip.StatusTrying
		case "200":
			return sip.StatusOK
		default:
			code, _ := strconv.Atoi(v)
			return code
		}
	}()

	if onNot != nil {
		onNot(code)
	} else if code >= 200 {
		_ = d.Close(context.TODO())
	}
}

func dialogHandleRefer(d Call, dg *Agent, req *Request, tx ServerTransaction, onReferDialog OnReferFunc) error {
	// https://datatracker.ietf.org/doc/html/rfc3515#section-2.4.2
	// 	An agent responding to a REFER method MUST return a 400 (Bad Request)
	//    if the request contained zero or more than one Refer-To header field
	//    values.
	log := dg.log
	referTo := req.GetHeader("Refer-To")
	if referTo == nil {
		log.Info("Received REFER without Refer-To header")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	referToUri := URI{}
	headerParams := sip.NewParams()

	_, err := sip.ParseAddressValue(referTo.Value(), &referToUri, &headerParams)
	if err != nil {
		log.Info("Received REFER but failed to parse Refer-To uri", "error", err)
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	contact := req.Contact()
	if contact == nil {
		log.Info("Received REFER but no Contact Header present")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", []byte("No Contact Header")))
	}

	// TODO can we locate this more checks
	log.Info("Accepting refer")
	if err := tx.Respond(sip.NewResponseFromRequest(req, 202, "Accepted", nil)); err != nil {
		return fmt.Errorf("failed to send 202 Accepted")
	}

	// This transcation can now terminate?
	return dialogReferInvite(d, dg, referToUri, contact.Address, onReferDialog, req)
}

func dialogReferInvite(d Call, dg *Agent, referToUri URI, remoteTarget URI, onReferDialog OnReferFunc, referReq *Request) error {
	raw, ok := d.(RawCall)
	if !ok {
		return fmt.Errorf("refer: %w", ErrRawCallUnsupported)
	}

	// TODO after this we could get BYE immediately, but caller would not be able
	// to take control over refer dialog

	// REFER State reasons summary
	/* | Reason | Meaning | Should Retry? | Automatic Retry? |
	   |--------|---------|---------------|------------------|
	   | **noresource** | Success or final failure | No (completed) | No |
	   | **rejected** | Policy denied | Maybe later | No |
	   | **deactivated** | Feature disabled | Yes, after retry-after | No |
	   | **probation** | Temporary failure | Yes, after retry-after | No |
	   | **timeout** | Request timed out | Maybe | No |
	   | **giveup** | Referee gave up | Maybe | No |	 */

	type subState struct {
		state      string
		reason     string
		expires    int
		retryAfter int
	}

	// TODO: Multiple Refers require having ID
	referID := ""

	sendNotify := func(ctx context.Context, statusCode int, reason string, sub subState) error {
		req := sip.NewRequest(sip.NOTIFY, remoteTarget)

		referParams := sip.NewParams()
		referParams.Add("refer", "")
		if referID != "" {
			referParams.Add("id", referID)
		}

		stateParams := sip.NewParams()
		stateParams.Add(sub.state, "")
		if sub.reason != "" {
			stateParams.Add("reason", sub.reason)
		}
		if sub.retryAfter > 0 {
			stateParams.Add("retry-after", strconv.Itoa(sub.retryAfter))
		}

		if sub.expires > 0 {
			stateParams.Add("expires", strconv.Itoa(sub.expires))
		}

		req.AppendHeader(sip.NewHeader("Event", referParams.ToString(';')))
		req.AppendHeader(sip.NewHeader("Subscription-State", stateParams.ToString(';')))
		req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
		frag := fmt.Sprintf("SIP/2.0 %d %s", statusCode, reason)
		req.SetBody([]byte(frag))

		res, err := raw.Do(ctx, req)
		if err != nil {
			return err
		}
		if res.StatusCode != 200 {
			return fmt.Errorf("notify received non 200 response. code=%d", res.StatusCode)
		}
		return nil
	}

	ctx := d.Context()

	// TODO  Mutliple Refers require IDs in NOTIFY event header
	// https://datatracker.ietf.org/doc/html/rfc3515#section-2.4.6

	// FROM, TO, CALLID must be same to make SUBSCRIBE working

	// onReferRequest(referToUri, )

	// Check is this REFER RFC 3892 compatible
	// https://datatracker.ietf.org/doc/html/rfc3892#autoid-3
	// if referredBy != nil {
	// 	opts.Headers = append(opts.Headers, sip.HeaderClone(referredBy))
	// }

	referDialog, err := dg.NewCall(referToUri)
	if err != nil {
		return err
	}
	defer func() { _ = referDialog.release() }()

	if h := referReq.GetHeader("referred-by"); h != nil {
		referDialog.sess.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}
	if h := referReq.GetHeader("replaces"); h != nil {
		referDialog.sess.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}

	// 	The final NOTIFY sent in response to a REFER MUST indicate
	//    the subscription has been "terminated" with a reason of "noresource".
	//    (The resource being subscribed to is the state of the referenced
	//    request).
	referDialog.sess.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateConfirmed || s == sip.DialogStateEnded {
			if err := sendNotify(ctx, 200, "OK", subState{
				state:  "terminated",
				reason: "noresource",
			}); err != nil {
				dg.log.Info("REFER Notify failed for 200", "error", err)
			}
		}
	})

	if err := sendNotify(ctx, 100, "Trying", subState{state: "active", expires: 60}); err != nil {
		// log.Info("REFER NOTIFY 100 failed to sent", "error", err)
		return fmt.Errorf("refer NOTIFY 100 failed to sent : %w", err)
	}

	// We send ref dialog to processing. After sending 200 OK this session will terminate
	if err := onReferDialog(referDialog); err != nil {
		// DO notify?
		dg.log.Info("OnReferDialog handling failed with", "error", err)
		var resErr *DialogResponseError
		if errors.As(err, &resErr) {
			return sendNotify(ctx, resErr.StatusCode(), resErr.Response().Reason, subState{
				state:  "terminated",
				reason: "noresource",
			})
		}

		// If call failed to be established but not yet confirmed
		state := referDialog.sess.LoadState()
		if state == 0 || state == sip.DialogStateEstablished {
			return sendNotify(ctx, 400, "Bad Request", subState{
				state:  "terminated",
				reason: "noresource",
			})
		}
		return err
	}
	// Now this dialog will receive BYE and it will terminate
	// We need to send this referDialog to control of caller
	return nil
}

// reInviteExchangeTransport abstracts the dialog operations needed by the
// shared re-INVITE exchange driver. Both OutboundCall and
// InboundCall satisfy it.
type reInviteExchangeTransport interface {
	Do(ctx context.Context, req *Request) (*Response, error)
	RemoteContact() *ContactHeader
	writeReInviteACK(ctx context.Context, target URI) error
}

// reInviteExchange sends a re-INVITE request and drives the exchange to
// completion, identically for client and server dialogs: it retries on 491
// Request Pending (RFC 3261 §14.1) with a randomized backoff and ACKs the
// 2xx at its Contact, falling back to the dialog's remote target when the
// response carries none. The final ACK is sent without options.
//
// Each side keeps its own ACK sender (client WriteAck, server WriteRequest):
// the observable behavior — one ACK, no options — is the same.
func reInviteExchange(ctx context.Context, t reInviteExchangeTransport, req *Request) (*Response, error) {
	for {
		res, err := t.Do(ctx, req.Clone())
		if err != nil {
			return nil, err
		}

		if !res.IsSuccess() {
			// Retry on 491 Request Pending (RFC 3261 §14.1) with a simplified
			// single-tier randomized backoff of 2.0-4.0s, instead of the RFC's
			// Call-ID-owner-dependent two tiers.
			if res.StatusCode == sip.StatusRequestPending {
				select {
				case <-time.After(time.Duration(2000+mrand.IntN(200)*10) * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			return nil, newDialogResponseError(res)
		}

		// ACK the 2xx at its Contact (RFC 3261 §13.2.1). A 2xx without Contact
		// is malformed but does occur in the wild — dereferencing it here used
		// to panic. Fall back to the dialog's remote target.
		ackContact := res.Contact()
		if ackContact == nil {
			ackContact = t.RemoteContact()
		}
		if ackContact == nil {
			return nil, fmt.Errorf("reinvite: 2xx has no Contact and dialog has no remote target to ack: %w", ErrRemoteContactMissing)
		}
		if err := t.writeReInviteACK(ctx, ackContact.Address); err != nil {
			return res, err
		}

		return res, nil
	}
}
