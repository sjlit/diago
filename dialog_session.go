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

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type DialogSession interface {
	ID() string
	Context() context.Context
	// Hangup terminates the dialog. Options allow customizing headers of the
	// outgoing BYE / decline response (ex. Reason header)
	Hangup(ctx context.Context, opts ...SignalOption) error
	Media() *DialogMedia
	DialogSIP() *sipgo.Dialog
	Do(ctx context.Context, req *sip.Request) (*sip.Response, error)
	Close() error
}

//
// Here are many common functions built for dialog
//

func dialogRefer(ctx context.Context, d DialogSession, recipient sip.Uri, referTo sip.Uri, refferedBy sip.Uri, params *SignalParams) error {
	if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
		return fmt.Errorf("can only be called on answered dialog")
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

	res, err := d.Do(ctx, req)
	if err != nil {
		return err
	}

	if res.StatusCode != sip.StatusAccepted {
		return sipgo.ErrDialogResponse{
			Res: res,
		}
	}
	return nil
}

func dialogHandleReferNotify(d DialogSession, req *sip.Request, tx sip.ServerTransaction) {
	// TODO how to know this is refer
	contentTypeHeader := req.ContentType()
	if contentTypeHeader == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}
	contentType := contentTypeHeader.Value()
	// For now very basic check
	if !strings.HasPrefix(contentType, "message/sipfrag") {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	frag := string(req.Body())
	if len(frag) < len("SIP/2.0 100 xx") {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))

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
		d.Hangup(context.TODO())
	}
}

func dialogHandleRefer(d DialogSession, dg *Diago, req *sip.Request, tx sip.ServerTransaction, onReferDialog OnReferDialogFunc) error {
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

	referToUri := sip.Uri{}
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

func dialogReferInvite(d DialogSession, dg *Diago, referToUri sip.Uri, remoteTarget sip.Uri, onReferDialog OnReferDialogFunc, referReq *sip.Request) error {

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

		res, err := d.Do(ctx, req)
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

	referDialog, err := dg.NewDialog(referToUri)
	if err != nil {
		return err
	}
	defer referDialog.Close()

	if h := referReq.GetHeader("referred-by"); h != nil {
		referDialog.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}
	if h := referReq.GetHeader("replaces"); h != nil {
		referDialog.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}

	// 	The final NOTIFY sent in response to a REFER MUST indicate
	//    the subscription has been "terminated" with a reason of "noresource".
	//    (The resource being subscribed to is the state of the referenced
	//    request).
	referDialog.OnState(func(s sip.DialogState) {
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
		var resErr *sipgo.ErrDialogResponse
		if errors.As(err, &resErr) {
			return sendNotify(ctx, resErr.Res.StatusCode, resErr.Res.Reason, subState{
				state:  "terminated",
				reason: "noresource",
			})
		}

		// If call failed to be established but not yet confirmed
		state := referDialog.LoadState()
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
// shared re-INVITE exchange driver. Both DialogClientSession and
// DialogServerSession satisfy it.
type reInviteExchangeTransport interface {
	Do(ctx context.Context, req *sip.Request) (*sip.Response, error)
	RemoteContact() *sip.ContactHeader
	writeReInviteACK(ctx context.Context, target sip.Uri) error
}

// reInviteExchange sends a re-INVITE request and drives the exchange to
// completion, identically for client and server dialogs: it retries on 491
// Request Pending (RFC 3261 §14.1) with a randomized backoff and ACKs the
// 2xx at its Contact, falling back to the dialog's remote target when the
// response carries none. The final ACK is sent without options.
//
// Each side keeps its own ACK sender (client WriteAck, server WriteRequest):
// the observable behavior — one ACK, no options — is the same.
func reInviteExchange(ctx context.Context, t reInviteExchangeTransport, req *sip.Request) (*sip.Response, error) {
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

			return nil, sipgo.ErrDialogResponse{
				Res: res,
			}
		}

		// ACK the 2xx at its Contact (RFC 3261 §13.2.1). A 2xx without Contact
		// is malformed but does occur in the wild — dereferencing it here used
		// to panic. Fall back to the dialog's remote target.
		ackContact := res.Contact()
		if ackContact == nil {
			ackContact = t.RemoteContact()
		}
		if ackContact == nil {
			return nil, fmt.Errorf("reinvite: 2xx has no Contact and dialog has no remote target to ACK")
		}
		if err := t.writeReInviteACK(ctx, ackContact.Address); err != nil {
			return res, err
		}

		return res, nil
	}
}
