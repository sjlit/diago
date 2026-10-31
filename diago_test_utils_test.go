// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// ConnRecorder is a sipgo connection usable without a real transport. A fresh
// recorder is created per client transaction, so each one is normally touched
// by a single goroutine; the mutex keeps WriteMsg race-free if the transaction
// layer ever writes from more than one.
type ConnRecorder struct {
	mu   sync.Mutex
	msgs []sip.Message

	ref atomic.Int32
}

func NewConnRecorder() *ConnRecorder {
	return &ConnRecorder{}
}

func (c *ConnRecorder) LocalAddr() net.Addr {
	return nil
}

func (c *ConnRecorder) WriteMsg(msg sip.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
	return nil
}
func (c *ConnRecorder) Ref(i int) int {
	return int(c.ref.Add(int32(i)))
}
func (c *ConnRecorder) TryClose() (int, error) {
	new := c.ref.Add(int32(-1))
	return int(new), nil
}
func (c *ConnRecorder) Close() error { return nil }

type clientTxRequester struct {
	// rec *siptest.ClientTxRecorder
	onRequest func(req *sip.Request) *sip.Response
}

func (r *clientTxRequester) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	key, _ := sip.ClientTxKeyMake(req)
	rec := NewConnRecorder()
	tx := sip.NewClientTx(key, req, rec, slog.Default())
	if err := tx.Init(); err != nil {
		return nil, err
	}

	resp := r.onRequest(req)
	go tx.Receive(resp)

	return tx, nil
}

// agentPort returns the port the agent's first transport actually bound.
// ServeBackground only returns once every listener is ready, so the resolved
// port can be read immediately afterwards; no polling is needed.
func agentPort(t *testing.T, dg *Agent) int {
	t.Helper()
	port := dg.transports[0].BindPort
	require.NotZero(t, port, "agent transport must be bound before its port is read")
	return port
}

// servingAgent builds an Agent, serves handler in the background and returns
// the agent together with the port its listener bound. Callers dial the
// returned port instead of a shared fixed one: fixed ports clash whenever two
// test binaries run on the same host (ex. go test ./... in two shells), which
// surfaces as "bind: address already in use".
//
// The port is known only after ServeBackground returns, so a serve handler
// that needs it (ex. to build a routable Contact) must read it through an
// atomic: the test goroutine writes the value while handler goroutines run
// concurrently, which the race detector flags without that edge (see
// bridge_test.go for the atomic.Int32 pattern).
func servingAgent(t *testing.T, ctx context.Context, h CallHandler, opts ...AgentOption) (*Agent, int) {
	t.Helper()
	dg, err := New(opts...)
	require.NoError(t, err)
	require.NoError(t, dg.ServeBackground(ctx, h))
	return dg, agentPort(t, dg)
}

// recvWithin receives one value from ch, failing the test instead of hanging
// forever when the value never arrives. Use it for channels fed by library
// callbacks: an unbuffered send there blocks the callback goroutine for the
// rest of the test binary, and an unbounded receive stalls the test until the
// global timeout without saying what it was waiting for.
func recvWithin[T any](t *testing.T, ch <-chan T, d time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("no value received after %s", d)
		var zero T
		return zero
	}
}

// canBindUDPAddr reports whether a UDP listener can bind addr. Some tests fork
// media onto extra loopback IPs (ex. 127.0.0.2) which are absent on hosts
// without a loopback alias (common macOS setup).
func canBindUDPAddr(addr *net.UDPAddr) bool {
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return false
	}
	return conn.Close() == nil
}

// ServeBackground is a test-only shim for the tests that predate
// Agent.Start. Production code uses Start and observes the returned
// ServerHandle; keeping the shim here avoids rewriting the whole suite while
// the public API is being reshaped.
func (dg *Agent) ServeBackground(ctx context.Context, h CallHandler) error {
	_, err := dg.Start(ctx, h)
	return err
}
