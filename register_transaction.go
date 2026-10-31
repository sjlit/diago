// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// RegisterResponseError reports a final non-200 REGISTER response.
type RegisterResponseError struct {
	registerReq *Request
	registerRes *Response
	msg         string
}

func newRegisterResponseError(req *Request, res *Response) *RegisterResponseError {
	msg := ""
	if res != nil {
		msg = res.StartLine()
	}
	return &RegisterResponseError{registerReq: req, registerRes: res, msg: msg}
}

// statusIntervalTooBrief is the REGISTER 423 final response (RFC 3261 §10.2.8),
// absent from sipgo's status constants.
const statusIntervalTooBrief = 423

// unregisterTimeout bounds the unregister sent when a registration loop exits,
// and the time Agent.Close waits for each registration.
const unregisterTimeout = 5 * time.Second

// StatusCode returns the SIP status code of the REGISTER response. It returns
// zero when the error was constructed without a response.
func (e *RegisterResponseError) StatusCode() int {
	if e == nil || e.registerRes == nil {
		return 0
	}
	return e.registerRes.StatusCode
}

func (e *RegisterResponseError) Error() string {
	if e == nil {
		return "register response error"
	}
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("register response: %d", e.StatusCode())
}

// RegisterRequest returns the REGISTER request that produced the error.
func (e *RegisterResponseError) RegisterRequest() *Request {
	if e == nil {
		return nil
	}
	return e.registerReq
}

// RegisterResponse returns the final REGISTER response.
func (e *RegisterResponseError) RegisterResponse() *Response {
	if e == nil {
		return nil
	}
	return e.registerRes
}

// RegisterTransaction is a REGISTER transaction: it owns the request
// template, credentials and expiry, and can register, qualify and
// unregister the contact. Agent.Register builds one and drives the
// re-registration loop.
type RegisterTransaction struct {
	register signalRegisterParams
	username string
	password string
	// mutateRequest is the transaction-level WithRequestMutator hook. It runs
	// on every request cloned from Origin, before per-call options.
	mutateRequest func(*Request) error
	Origin        *Request

	client *Client
	log    *slog.Logger

	// expiry tracks the server's Expires value for the current registration.
	// It is runtime state, not configuration: doRegister refreshes it from the
	// response.
	expiry time.Duration
	// retryAfter carries the server-requested Retry-After delay for the next
	// attempt. It is set by the retry orchestration (Agent.Register) after a
	// retryable failure and cleared on the next successful REGISTER.
	retryAfter time.Duration
	// after is the timer factory used between REGISTER attempts. It is a
	// field so tests can drive the retry loop without wall-clock waits.
	after func(time.Duration) <-chan time.Time
}

func newRegisterTransaction(client *Client, recipient URI, contact ContactHeader, log *slog.Logger, params *signalParams, after func(time.Duration) <-chan time.Time) *RegisterTransaction {
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
func (t *RegisterTransaction) cloneOrigin() (*Request, error) {
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
func (t *RegisterTransaction) Register(ctx context.Context, opts ...RegisterAttemptOption) error {
	params, err := newSignalParams(opts...)
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

	res, err := func() (*Response, error) {
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
		return newRegisterResponseError(req, res)
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

// QualifyLoop refreshes the registration on the negotiated expiry until
// ctx is canceled or a terminal failure occurs (see Agent.Register).
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
// 501/505. 423 Interval Too Brief is handled separately by Agent.Register
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
func (t *RegisterTransaction) Unregister(ctx context.Context, opts ...RegisterAttemptOption) error {
	params, err := newSignalParams(opts...)
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
func (t *RegisterTransaction) Qualify(ctx context.Context, opts ...RegisterAttemptOption) error {
	params, err := newSignalParams(opts...)
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

func (t *RegisterTransaction) doRequest(ctx context.Context, req *Request, params *signalParams) error {
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
		return newRegisterResponseError(req, res)
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

// ErrRegistrationClosed is returned by Registration.WaitReady when the handle
// was closed (or the loop stopped) before the first successful REGISTER.
var ErrRegistrationClosed = errors.New("registration closed")

// Registration is a running REGISTER loop created by Agent.Register. The
// initial REGISTER and every re-registration run in a background goroutine;
// the handle owns their lifecycle:
//
//   - Ready is closed after the first successful REGISTER.
//   - Done is closed when the loop ends (terminal response, ctx cancellation
//     or Close).
//   - Err reports the loop's terminal error (nil when Close stopped it).
//   - Wait blocks until the loop ends and returns Err.
//   - WaitReady waits for Ready, Done or ctx.
//   - Close stops the loop, unregisters the contact and waits for the loop to
//     exit (bounded by ctx). It is idempotent.
//
// Agent.Close also closes the handle, unless it was created with
// WithRegistrationDetached; then the caller owns it.
//
// The handle is safe for concurrent use.
type Registration struct {
	log   *slog.Logger
	trans *RegisterTransaction

	ctx    context.Context
	cancel context.CancelFunc

	// onDone, when set, runs once the loop has unregistered and exited. The
	// Agent uses it to stop tracking the handle.
	onDone func(*Registration)

	ready chan struct{}
	done  chan struct{}

	mu     sync.Mutex
	err    error
	closed bool
	once   sync.Once
}

func newRegistration(ctx context.Context, log *slog.Logger, t *RegisterTransaction) *Registration {
	runCtx, cancel := context.WithCancel(ctx)
	r := &Registration{
		log:    log,
		trans:  t,
		ctx:    runCtx,
		cancel: cancel,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	return r
}

// start runs the registration loop in the background.
func (r *Registration) start() {
	go r.run()
}

// Ready is closed after the first successful REGISTER. It is never closed
// when the initial registration fails terminally; observe Done in that case.
func (r *Registration) Ready() <-chan struct{} {
	return r.ready
}

// Done is closed when the registration loop exits.
func (r *Registration) Done() <-chan struct{} {
	return r.done
}

// Err reports the loop's terminal error. It returns nil while the loop is
// running and after a clean Close.
func (r *Registration) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Wait blocks until the registration loop exits and returns its terminal
// error (nil for a clean Close).
func (r *Registration) Wait() error {
	<-r.done
	return r.Err()
}

// WaitReady waits until the first REGISTER succeeded, the loop terminated, or
// ctx is done. When the handle was closed (or the loop stopped) before the
// first success it returns the terminal error, or ErrRegistrationClosed for a
// clean Close.
func (r *Registration) WaitReady(ctx context.Context) error {
	select {
	case <-r.ready:
		return nil
	case <-r.done:
		if err := r.Err(); err != nil {
			return err
		}
		return ErrRegistrationClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the loop, waits for the deferred unregister to complete and
// returns the wait error (nil unless ctx expired first). It is idempotent and
// safe for concurrent use.
func (r *Registration) Close(ctx context.Context) error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancel()
	})
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isClosed reports whether Close was called.
func (r *Registration) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// setErr records the first terminal error of the loop.
func (r *Registration) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

// run executes the initial REGISTER and the re-registration loop, replicating
// the historical pacing/retry rules (see Register), then unregisters before
// Done is closed.
func (r *Registration) run() {
	defer close(r.done)
	if r.onDone != nil {
		defer r.onDone(r)
	}
	// unregister is only meaningful once a REGISTER succeeded: on an initial
	// failure there is no binding to remove, so exit without an extra request
	// (matching the historical behavior).
	unregister := false
	defer func() {
		if !unregister {
			return
		}
		// The timeout context is independent of r.ctx on purpose.
		ctx, cancel := context.WithTimeout(context.Background(), unregisterTimeout)
		defer cancel()
		if err := r.trans.Unregister(ctx); err != nil {
			r.log.Error("Failed to unregister", "error", err)
			return
		}
		r.log.Debug("Unregister successfull")
	}()

	t := r.trans
	if err := t.Register(r.ctx); err != nil {
		if r.isClosed() && errors.Is(err, context.Canceled) {
			// Close canceled the loop: a clean stop, not a failure.
			return
		}
		r.setErr(err)
		return
	}
	unregister = true
	close(r.ready)

	for {
		r.log.Debug("Qualify sent")
		err := t.QualifyLoop(r.ctx)

		retryAfter := 5 * time.Second
		var rr *RegisterResponseError
		if errors.As(err, &rr) {
			resCode := rr.registerRes.StatusCode
			switch {
			case resCode == statusIntervalTooBrief:
				// RFC 3261 §10.2.8: retry with an Expires of at least
				// Min-Expires instead of treating the response as terminal.
				minExpires, ok := minExpiresDelay(rr.registerRes)
				if !ok {
					r.setErr(err)
					return
				}
				if t.register.Expiry >= minExpires {
					// We already advertise at least Min-Expires, so another
					// attempt cannot satisfy the server; repeated 423 must not
					// become a 1s retry loop.
					r.setErr(err)
					return
				}
				t.setExpiry(minExpires)
				retryAfter = time.Second
			case shouldRetryRegister(resCode):
				// Retry-After is honored below.
			default:
				r.setErr(err)
				return
			}

			if h := rr.registerRes.GetHeader("Retry-After"); h != nil {
				ra, _ := strconv.Atoi(h.Value())
				if ra > 0 {
					retryAfter = time.Duration(ra) * time.Second
				}
			}
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if r.isClosed() && errors.Is(err, context.Canceled) {
				// Close canceled the loop: a clean stop, not a failure.
				return
			}
			r.setErr(err)
			return
		}

		// Retry pacing: explicit RetryInterval > Retry-After value > the
		// default computed from the negotiated expiry (see calcRetry).
		t.retryAfter = retryAfter
	}
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
func minExpiresDelay(res *Response) (time.Duration, bool) {
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
