// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingAfter returns an after-factory that records every requested delay
// and fires immediately, so retry loops run without wall-clock waits.
func recordingAfter(delays *[]time.Duration, mu *sync.Mutex) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		mu.Lock()
		*delays = append(*delays, d)
		mu.Unlock()

		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
}

func TestRegisterCalcRetryPriority(t *testing.T) {
	t.Run("retry after wins over expiry", func(t *testing.T) {
		rtx := &RegisterTransaction{expiry: 100 * time.Second, retryAfter: 7 * time.Second}
		assert.Equal(t, 7*time.Second, rtx.calcRetry(rtx.expiry))
	})

	t.Run("explicit interval wins over retry after", func(t *testing.T) {
		rtx := &RegisterTransaction{
			expiry:     100 * time.Second,
			retryAfter: 7 * time.Second,
			register:   signalRegisterParams{RetryInterval: 3 * time.Millisecond},
		}
		assert.Equal(t, 3*time.Millisecond, rtx.calcRetry(rtx.expiry))
	})

	t.Run("expiry derived", func(t *testing.T) {
		rtx := &RegisterTransaction{expiry: 100 * time.Second}
		assert.Equal(t, 75*time.Second, rtx.calcRetry(rtx.expiry))
	})

	t.Run("default when nothing known", func(t *testing.T) {
		rtx := &RegisterTransaction{}
		assert.Equal(t, 30*time.Second, rtx.calcRetry(0))
	})
}

func TestRegisterRetryAfterHonored(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2:
			res := sip.NewResponseFromRequest(req, 408, "Request Timeout", nil)
			res.AppendHeader(sip.NewHeader("Retry-After", "7"))
			return res
		default:
			// Third attempt: end the loop from the consumer side.
			cancel()
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.ErrorIs(t, reg.Wait(), context.Canceled)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(delays), 2, "expected at least two scheduled intervals")
	assert.Equal(t, 7*time.Second, delays[1], "Retry-After must be honored verbatim")
	assert.GreaterOrEqual(t, attempts.Load(), int32(3))
}

func TestRegisterRetryIntervalBeatsRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2:
			res := sip.NewResponseFromRequest(req, 408, "Request Timeout", nil)
			res.AppendHeader(sip.NewHeader("Retry-After", "7"))
			return res
		default:
			cancel()
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"},
		RegisterOptions{RetryInterval: 3 * time.Millisecond})
	require.NoError(t, err)
	require.ErrorIs(t, reg.Wait(), context.Canceled)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(delays), 2)
	assert.Equal(t, 3*time.Millisecond, delays[1],
		"an explicit retry interval must override Retry-After")
}

func TestRegisterTransient408Retries(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2, 3:
			return sip.NewResponseFromRequest(req, 408, "Request Timeout", nil)
		default:
			cancel()
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	done := make(chan error, 1)
	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	go func() {
		done <- reg.Wait()
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not return after context cancellation")
	}
	assert.GreaterOrEqual(t, attempts.Load(), int32(3), "408 must be retried")
}

func TestRegisterTerminal4xxStops(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		// The deferred Unregister at the end of Agent.Register sends
		// Expires: 0; it must not count as a retry.
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		default:
			return sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	done := make(chan error, 1)
	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	go func() {
		done <- reg.Wait()
	}()

	select {
	case err := <-done:
		var rr *RegisterResponseError
		require.ErrorAs(t, err, &rr)
		assert.Equal(t, 403, rr.StatusCode())
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not stop on a terminal 4xx")
	}
	assert.EqualValues(t, 2, attempts.Load(), "terminal 4xx must not be retried")
}

func TestShouldRetryRegister(t *testing.T) {
	cases := []struct {
		code  int
		retry bool
	}{
		{100, false},
		{200, false},
		{301, false},
		{302, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{407, false},
		{408, true},
		{480, false},
		{486, false},
		{491, true},
		{500, true},
		{501, false},
		{502, true},
		{503, true},
		{504, true},
		{505, false},
		{599, true},
		{600, false},
		{603, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.retry, shouldRetryRegister(c.code), "code=%d", c.code)
	}
}

// TestRegisterRetryable5xxKeepsRetrying covers the classification hole the
// old switch had: 500 and 503 are transient and must be retried.
func TestRegisterRetryable5xxKeepsRetrying(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2:
			return sip.NewResponseFromRequest(req, 500, "Server Internal Error", nil)
		case 3:
			return sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)
		default:
			cancel()
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.ErrorIs(t, reg.Wait(), context.Canceled)
	assert.GreaterOrEqual(t, attempts.Load(), int32(4), "500 and 503 must be retried")
}

// TestRegisterTerminal5xxStops: 501 is not transient, the loop must stop.
func TestRegisterTerminal5xxStops(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		default:
			return sip.NewResponseFromRequest(req, 501, "Not Implemented", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	done := make(chan error, 1)
	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	go func() {
		done <- reg.Wait()
	}()

	select {
	case err := <-done:
		var rr *RegisterResponseError
		require.ErrorAs(t, err, &rr)
		assert.Equal(t, 501, rr.StatusCode())
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not stop on 501")
	}
	assert.EqualValues(t, 2, attempts.Load(), "501 must not be retried")
}

// TestRegister3xxStops: redirects are terminal until Contact following exists.
func TestRegister3xxStops(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		default:
			return sip.NewResponseFromRequest(req, 302, "Moved Temporarily", nil)
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	done := make(chan error, 1)
	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	go func() {
		done <- reg.Wait()
	}()

	select {
	case err := <-done:
		var rr *RegisterResponseError
		require.ErrorAs(t, err, &rr)
		assert.Equal(t, 302, rr.StatusCode())
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not stop on a 3xx")
	}
	assert.EqualValues(t, 2, attempts.Load(), "3xx must not be retried")
}

// Regression: a transient failure's Retry-After override was sticky — it was
// cleared only by the initial doRegister, not by a successful Qualify, so the
// healthy re-registration pacing stayed pinned to it forever.
func TestRegisterRetryAfterClearedOnSuccess(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		switch n := attempts.Add(1); n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2:
			res := sip.NewResponseFromRequest(req, 408, "Request Timeout", nil)
			res.AppendHeader(sip.NewHeader("Retry-After", "7"))
			return res
		default:
			res := sip.NewResponseFromRequest(req, 200, "OK", nil)
			res.AppendHeader(sip.NewHeader("Expires", "3600"))
			return res
		}
	})

	delaysCh := make(chan time.Duration, 8)
	dg.registerAfter = func(d time.Duration) <-chan time.Time {
		delaysCh <- d
		ch := make(chan time.Time, 1)
		if d < time.Minute {
			// Fast-forward the transient retry pacing.
			ch <- time.Now()
		}
		return ch
	}

	done := make(chan error, 1)
	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	go func() {
		done <- reg.Wait()
	}()

	var delays []time.Duration
	for i := 0; i < 3; i++ {
		select {
		case d := <-delaysCh:
			delays = append(delays, d)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for delay %d, got %v", i+1, delays)
		}
	}
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Register did not return after cancel")
	}

	require.Len(t, delays, 3)
	assert.Equal(t, 30*time.Second, delays[0], "initial pacing comes from the unset expiry")
	assert.Equal(t, 7*time.Second, delays[1], "Retry-After must be honored once")
	assert.Equal(t, 45*time.Minute, delays[2], "a successful Qualify must recompute pacing from Expires")
}

// 423 Interval Too Brief must not be terminal: the retry has to advertise at
// least Min-Expires (RFC 3261 §10.2.8).
func TestRegisterIntervalTooBriefUsesMinExpires(t *testing.T) {
	var attempts atomic.Int32
	var retryExpires atomic.Value // string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		n := attempts.Add(1)
		if n == 3 {
			if h := req.GetHeader("Expires"); h != nil {
				retryExpires.Store(h.Value())
			}
		}
		switch n {
		case 1:
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		case 2:
			res := sip.NewResponseFromRequest(req, 423, "Interval Too Brief", nil)
			res.AppendHeader(sip.NewHeader("Min-Expires", "120"))
			return res
		default:
			cancel()
			res := sip.NewResponseFromRequest(req, 200, "OK", nil)
			res.AppendHeader(sip.NewHeader("Expires", "120"))
			return res
		}
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.ErrorIs(t, reg.Wait(), context.Canceled)

	got, _ := retryExpires.Load().(string)
	assert.Equal(t, "120", got, "retry after 423 must carry Min-Expires")
	require.GreaterOrEqual(t, attempts.Load(), int32(3))
}

// TestRegistrationHandle locks the Registration handle contract: Ready closes
// after the first successful REGISTER, Close stops the loop and unregisters
// the contact, and Wait reports a clean stop.
func TestRegistrationHandle(t *testing.T) {
	var mu sync.Mutex
	var reqs []*sip.Request
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		reqs = append(reqs, req.Clone())
		mu.Unlock()
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg, err := dg.StartRegistration(ctx, "sip:alice@localhost")
	require.NoError(t, err)
	require.NoError(t, reg.WaitReady(ctx))
	require.NoError(t, reg.Close(ctx))
	require.NoError(t, reg.Wait(), "a clean Close must not report a loop error")

	select {
	case <-reg.Ready():
	default:
		t.Fatal("Ready must be closed after the first successful REGISTER")
	}
	select {
	case <-reg.Done():
	default:
		t.Fatal("Done must be closed after Close")
	}

	mu.Lock()
	defer mu.Unlock()
	var unregister bool
	for _, req := range reqs {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			unregister = true
		}
	}
	assert.True(t, unregister, "Close must send an unregister (Expires: 0)")
}

// TestRegistrationInitialFailure locks that a terminal initial REGISTER fails
// the handle without ever reporting Ready, and that no unregister is sent when
// nothing was ever registered.
func TestRegistrationInitialFailure(t *testing.T) {
	var attempts atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		attempts.Add(1)
		return sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)

	err = reg.WaitReady(ctx)
	var rr *RegisterResponseError
	require.ErrorAs(t, err, &rr)
	assert.Equal(t, 403, rr.StatusCode())
	require.ErrorAs(t, reg.Wait(), &rr)

	select {
	case <-reg.Ready():
		t.Fatal("Ready must not be closed after a failed initial REGISTER")
	default:
	}
	assert.EqualValues(t, 1, attempts.Load(), "a failed initial REGISTER must not trigger an unregister")
}

// TestRegistrationCloseBeforeReady locks that closing a handle before the
// first success reports ErrRegistrationClosed from WaitReady instead of
// looking like a success.
func TestRegistrationCloseBeforeReady(t *testing.T) {
	r := &Registration{
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
	close(r.done) // loop stopped before Ready

	assert.ErrorIs(t, r.WaitReady(context.Background()), ErrRegistrationClosed)
}

// A server that keeps answering 423 with the same Min-Expires after the client
// already advertises it must not cause an endless 1s retry loop.
func TestRegisterIntervalTooBriefRepeatedIsTerminal(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		// The deferred Unregister must not count.
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		n := attempts.Add(1)
		if n == 1 {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
		res := sip.NewResponseFromRequest(req, 423, "Interval Too Brief", nil)
		res.AppendHeader(sip.NewHeader("Min-Expires", "120"))
		return res
	})

	var mu sync.Mutex
	var delays []time.Duration
	dg.registerAfter = recordingAfter(&delays, &mu)

	reg, err := dg.StartRegistrationURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	err = reg.Wait()
	var rr *RegisterResponseError
	require.ErrorAs(t, err, &rr, "a repeated 423 cannot be satisfied and must stop")
	assert.Equal(t, 423, rr.StatusCode())
	assert.EqualValues(t, 3, attempts.Load(), "one retry is attempted, then the loop stops")
}
