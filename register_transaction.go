// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type RegisterResponseError struct {
	RegisterReq *sip.Request
	RegisterRes *sip.Response

	Msg string
}

// statusIntervalTooBrief is the REGISTER 423 final response (RFC 3261 §10.2.8),
// absent from sipgo's status constants.
const statusIntervalTooBrief = 423

func (e *RegisterResponseError) StatusCode() int {
	return e.RegisterRes.StatusCode
}

func (e RegisterResponseError) Error() string {
	return e.Msg
}

type RegisterTransaction struct {
	register signalRegisterParams
	username string
	password string
	// mutateRequest is the transaction-level WithRequestMutator hook. It runs
	// on every request cloned from Origin, before per-call options.
	mutateRequest func(*sip.Request) error
	Origin        *sip.Request

	client *sipgo.Client
	log    *slog.Logger

	// expiry tracks the server's Expires value for the current registration.
	// It is runtime state, not configuration: doRegister refreshes it from the
	// response.
	expiry time.Duration
	// retryAfter carries the server-requested Retry-After delay for the next
	// attempt. It is set by the retry orchestration (Diago.Register) after a
	// retryable failure and cleared on the next successful REGISTER.
	retryAfter time.Duration
	// after is the timer factory used between REGISTER attempts. It is a
	// field so tests can drive the retry loop without wall-clock waits.
	after func(time.Duration) <-chan time.Time
}

func newRegisterTransaction(client *sipgo.Client, recipient sip.Uri, contact sip.ContactHeader, log *slog.Logger, params *signalParams, after func(time.Duration) <-chan time.Time) *RegisterTransaction {
	if params == nil {
		params = &signalParams{}
	}
	if after == nil {
		after = time.After
	}
	reg := params.Register
	expiry, allowHDRS := reg.Expiry, reg.AllowHeaders
	// log := p.getLoggerCtx(ctx, "Register")
	req := sip.NewRequest(sip.REGISTER, recipient)
	if params.Msg.Contact != nil {
		// Per registration Contact override
		req.AppendHeader(params.Msg.Contact.Clone())
	} else {
		req.AppendHeader(&contact)
	}

	if reg.ProxyHost != "" {
		req.SetDestination(reg.ProxyHost)
	}
	if expiry > 0 {
		expires := sip.ExpiresHeader(expiry.Seconds())
		req.AppendHeader(&expires)
	}
	if allowHDRS != nil {
		req.AppendHeader(sip.NewHeader("Allow", strings.Join(allowHDRS, ", ")))
	}
	for _, h := range params.Msg.Headers {
		if h == nil {
			continue
		}
		req.AppendHeader(h)
	}

	username := params.Dialog.Username
	if username == "" {
		username = client.Name()
	}

	t := &RegisterTransaction{
		Origin:        req, // origin maybe updated after first register
		register:      reg,
		username:      username,
		password:      params.Dialog.Password,
		mutateRequest: params.Msg.MutateRequest,
		client:        client,
		log:           log.With("caller", "Register"),
		after:         after,
	}

	return t
}

// cloneOrigin clones the transaction request template, applying the
// transaction-level WithRequestMutator hook so it shapes every REGISTER of
// this transaction. Per-call options are applied afterwards and keep the last
// word.
func (t *RegisterTransaction) cloneOrigin() (*sip.Request, error) {
	req := t.Origin.Clone()
	if t.mutateRequest == nil {
		return req, nil
	}
	if err := t.mutateRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// Register sends the initial REGISTER. Options allow customizing Contact,
// extra headers and the final request of this registration attempt.
// Scope: WithHeaders, WithContact, WithRequestMutator, WithAuthCredentials;
// credentials fall back to the transaction ones when not provided via options.
func (t *RegisterTransaction) Register(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Register", scopeRegisterAttempt)
	if err != nil {
		return err
	}
	if err := t.doRegister(ctx, params); err != nil {
		return err
	}

	if t.register.OnRegistered != nil {
		t.register.OnRegistered()
	}
	return nil
}

// signalCredentials overlays credentials passed via WithAuthCredentials on top
// of the transaction ones. Empty SignalOption fields fall back to the
// transaction values.
func (t *RegisterTransaction) signalCredentials(params *signalParams) (string, string) {
	username, password := t.username, t.password
	if params != nil {
		if params.Dialog.Username != "" {
			username = params.Dialog.Username
		}
		if params.Dialog.Password != "" {
			password = params.Dialog.Password
		}
	}
	return username, password
}

func (t *RegisterTransaction) doRegister(ctx context.Context, params *signalParams) error {
	username, password := t.signalCredentials(params)
	client := t.client
	req, err := t.cloneOrigin()
	if err != nil {
		return err
	}
	if err := applyRequestSignal(req, params); err != nil {
		return err
	}
	// Hold a reference to req's Contact header. Mutations made through this
	// variable persist on req itself, so a subsequent DigestAuth retry (line 210)
	// also sees the NAT-learned address.
	contact := req.Contact()

	res, err := func() (*sip.Response, error) {
		if contact.Address.Port == 0 && net.ParseIP(contact.Address.Host) != nil {

			if err := sipgo.ClientRequestRegisterBuild(client, req); err != nil {
				return nil, err
			}

			tx, err := client.TransactionLayer().NewClientTransaction(ctx, req)
			if err != nil {
				return nil, err
			}

			host, port, err := sip.ParseAddr(tx.Connection().LocalAddr().String())
			if err != nil {
				return nil, err
			}
			contact.Address.Host = host
			contact.Address.Port = port

			err = tx.Init()
			if err != nil {
				tx.Terminate()
				return nil, err
			}

			for {
				select {
				case res := <-tx.Responses():
					if res.IsProvisional() {
						continue
					}
					return res, nil

				case <-tx.Done():
					return nil, tx.Err()

				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		return client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	}()
	if err != nil {
		return fmt.Errorf("fail to create transaction req=%q: %w", req.StartLine(), err)
	}

	via := res.Via()
	if via == nil {
		return fmt.Errorf("no Via header in response")
	}

	// https://datatracker.ietf.org/doc/html/rfc3581#section-9
	if rport, _ := via.Params.Get("rport"); rport != "" {
		if p, err := strconv.Atoi(rport); err == nil {
			contact.Address.Port = p
		}

		if received, _ := via.Params.Get("received"); received != "" {
			// TODO: consider parsing IP
			contact.Address.Host = received
		}

		// contact is req's actual Contact header, so the mutations above
		// already updated the in-flight request. Propagate the learned
		// contact to origin so the next register reuses it.
		t.Origin.ReplaceHeader(contact.Clone())
	}

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	// Now update server expiry
	t.expiry = t.register.Expiry
	if h := res.GetHeader("Expires"); h != nil {
		val, err := strconv.Atoi(h.Value())
		if err != nil {
			return fmt.Errorf("failed to parse server Expires value: %w", err)
		}
		t.expiry = time.Duration(val) * time.Second
	}
	// The attempt succeeded: a Retry-After override from a previous failure
	// must not leak into the healthy re-registration pacing.
	t.retryAfter = 0

	return nil
}

func (t *RegisterTransaction) QualifyLoop(ctx context.Context) error {
	// t.expiry tracks the server's Expires response header (set by Qualify)
	// and is re-read on every loop iteration, so server-driven expiry
	// changes adjust the retry pacing automatically.
	retry := t.calcRetry(t.expiry)
	return t.reregisterLoop(ctx, retry)
}

func (t *RegisterTransaction) reregisterLoop(ctx context.Context, retry time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.after(retry):
		}

		prevExpiry := t.expiry
		if err := t.Qualify(ctx); err != nil {
			return err
		}

		// A successful Qualify refreshes expiry (and clears any Retry-After
		// override). Recompute the pacing whenever either changed so a
		// transient failure's delay does not become sticky.
		newRetry := t.calcRetry(t.expiry)
		if t.expiry != prevExpiry || newRetry != retry {
			t.log.Info("Register retry interval changed",
				"expiry", t.expiry, "retry", newRetry, "retry_old", retry)
			retry = newRetry
		}
	}
}

func (t *RegisterTransaction) calcRetry(expiry time.Duration) time.Duration {
	// Explicit caller interval has the final word.
	if t.register.RetryInterval != 0 {
		return t.register.RetryInterval
	}

	// Server-requested Retry-After is next; it is honored as given.
	if t.retryAfter > 0 {
		return t.retryAfter
	}

	calc := expiry.Seconds() * 0.75
	retry := time.Duration(calc) * time.Second

	// Set to 30 in case retry is not set
	if retry == 0 {
		retry = 30 * time.Second
	}

	return retry
}

// shouldRetryRegister reports whether a final (non-200) REGISTER response is
// transient and the re-registration loop should schedule another attempt.
//
// Retryable: 408 Request Timeout, 491 Request Pending, and 5xx except
// 501/505. 423 Interval Too Brief is handled separately by Diago.Register
// (RFC 3261 §10.2.8: retry with the Min-Expires value). Terminal: 3xx, every
// other 4xx (401/407 in particular: credentials are transaction-scoped, so
// retrying only repeats the same failure), 6xx and anything outside the SIP
// error space. `Retry-After` is honored on every retryable response by the
// caller.
func shouldRetryRegister(code int) bool {
	switch {
	case code == sip.StatusRequestTimeout || code == sip.StatusRequestPending:
		return true
	case code >= 500 && code <= 599:
		return code != sip.StatusNotImplemented && code != sip.StatusVersionNotSupported
	default:
		return false
	}
}

// Unregister unregisters the contact. Options allow customizing the request.
// Scope: WithHeaders, WithContact, WithRequestMutator, WithAuthCredentials.
func (t *RegisterTransaction) Unregister(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Unregister", scopeRegisterAttempt)
	if err != nil {
		return err
	}
	req, err := t.cloneOrigin()
	if err != nil {
		return err
	}

	req.RemoveHeader("Expires")
	req.RemoveHeader("Contact")
	req.AppendHeader(sip.NewHeader("Contact", "*"))
	expires := sip.ExpiresHeader(0)
	req.AppendHeader(&expires)
	if err := applyRequestSignal(req, params); err != nil {
		return err
	}
	return t.doRequest(ctx, req, params)
}

// Qualify refreshes the registration. Options allow customizing the request.
// Scope: WithHeaders, WithContact, WithRequestMutator, WithAuthCredentials.
func (t *RegisterTransaction) Qualify(ctx context.Context, opts ...SignalOption) error {
	params, err := newSignalParams(opts, "Qualify", scopeRegisterAttempt)
	if err != nil {
		return err
	}
	req, err := t.cloneOrigin()
	if err != nil {
		return err
	}
	if err := applyRequestSignal(req, params); err != nil {
		return err
	}
	return t.doRequest(ctx, req, params)
}

func (t *RegisterTransaction) doRequest(ctx context.Context, req *sip.Request, params *signalParams) error {
	// log := p.getLoggerCtx(ctx, "Register")
	client := t.client
	username, password := t.signalCredentials(params)
	// Send request and parse response
	// req.SetDestination(*dst)
	req.RemoveHeader("Via")
	res, err := client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
	}

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	// Check is expirese changed
	if h := res.GetHeader("Expires"); h != nil {
		val, err := strconv.Atoi(h.Value())
		if err != nil {
			return fmt.Errorf("failed to parse server Expires value: %w", err)
		}
		t.expiry = time.Duration(val) * time.Second
	}

	// The attempt succeeded: a Retry-After override from a previous failure
	// must not leak into the healthy re-registration pacing (calcRetry would
	// otherwise keep returning the stale value forever).
	t.retryAfter = 0

	return nil
}

// setExpiry raises the registration expiry used by subsequent attempts and
// updates the Origin request template so they advertise it. It implements the
// RFC 3261 §10.2.8 reaction to 423 Interval Too Brief / Min-Expires.
func (t *RegisterTransaction) setExpiry(d time.Duration) {
	t.register.Expiry = d
	expires := sip.ExpiresHeader(d.Seconds())
	t.Origin.RemoveHeader("Expires")
	t.Origin.AppendHeader(&expires)
}

// minExpiresDelay extracts a positive, representable Min-Expires value from a
// 423 response.
func minExpiresDelay(res *sip.Response) (time.Duration, bool) {
	h := res.GetHeader("Min-Expires")
	if h == nil {
		return 0, false
	}
	sec, err := strconv.Atoi(strings.TrimSpace(h.Value()))
	// Reject non-positive and absurd values: the seconds->duration conversion
	// must not overflow, and sip.ExpiresHeader is a uint32 seconds value.
	if err != nil || sec <= 0 || sec > math.MaxInt32 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}
