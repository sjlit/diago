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
	t.Run("RetryAfterWinsOverExpiry", func(t *testing.T) {
		rtx := &RegisterTransaction{expiry: 100 * time.Second, retryAfter: 7 * time.Second}
		assert.Equal(t, 7*time.Second, rtx.calcRetry(rtx.expiry))
	})

	t.Run("ExplicitIntervalWinsOverRetryAfter", func(t *testing.T) {
		rtx := &RegisterTransaction{
			expiry:     100 * time.Second,
			retryAfter: 7 * time.Second,
			register:   SignalRegisterParams{RetryInterval: 3 * time.Millisecond},
		}
		assert.Equal(t, 3*time.Millisecond, rtx.calcRetry(rtx.expiry))
	})

	t.Run("ExpiryDerived", func(t *testing.T) {
		rtx := &RegisterTransaction{expiry: 100 * time.Second}
		assert.Equal(t, 75*time.Second, rtx.calcRetry(rtx.expiry))
	})

	t.Run("DefaultWhenNothingKnown", func(t *testing.T) {
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

	err := dg.Register(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

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

	err := dg.Register(ctx, sip.Uri{User: "alice", Host: "localhost"},
		WithRegisterRetryInterval(3*time.Millisecond))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

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
	go func() {
		done <- dg.Register(ctx, sip.Uri{User: "alice", Host: "localhost"})
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
		// The deferred Unregister at the end of Diago.Register sends
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
	go func() {
		done <- dg.Register(ctx, sip.Uri{User: "alice", Host: "localhost"})
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
