// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shutdownTestSDP() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FormatTypeALaw}, "")
}

func cacheCountClientDialogs(dg *Agent) int {
	n := 0
	dg.cache.client.rangeDialogs(context.Background(), func(id string, d *OutboundCall) bool {
		n++
		return true
	})
	return n
}

// TestDiagoShutdownTearsDownClientDialogs covers Close phase 1 without real
// sockets: a confirmed client dialog must be hung up (BYE observed), closed,
// evicted from the cache, and further serving must be refused.
func TestDiagoShutdownTearsDownClientDialogs(t *testing.T) {
	var byeCount atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.BYE {
			byeCount.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.Equal(t, 1, cacheCountClientDialogs(dg), "confirmed dialog must be tracked")

	require.NoError(t, dg.Close(ctx))
	assert.Equal(t, int32(1), byeCount.Load(), "Close must send BYE to confirmed dialogs")
	assert.Equal(t, 0, cacheCountClientDialogs(dg), "Close must evict dialogs from the cache")

	// Idempotent.
	require.NoError(t, dg.Close(ctx))

	// Serving after Close is refused.
	err = dg.serve(ctx, func(ctx context.Context, d *InboundCall) error {
		return nil
	}, func() {})
	require.Error(t, err)
	assert.ErrorContains(t, err, "shut down")

}

// TestDiagoShutdownStopsListeners covers phase 2 with real UDP listeners.
func TestDiagoShutdownStopsListeners(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := newAgent(withUserAgent(ua), withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, dg.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		return nil
	}))
	require.NotZero(t, dg.transports[0].BindPort, "listener must have bound an ephemeral port")
	assert.True(t, dg.serving.Load())

	require.NoError(t, dg.Close(ctx))
	assert.False(t, dg.serving.Load(), "listeners must be stopped after Close")

	// Already-serving must be rejected while up.
	dg2, err := newAgent(withUserAgent(ua), withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, err)
	require.NoError(t, dg2.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		return nil
	}))
	t.Cleanup(func() { dg2.Close(context.Background()) })
	err = dg2.serve(ctx, func(ctx context.Context, d *InboundCall) error {
		return nil
	}, func() {})
	require.Error(t, err)
	assert.ErrorContains(t, err, "already serving")
}

// TestShutdownUnregistersRegistrations locks that Agent.Close unregisters the
// registrations it created while the signaling stack is still up, stops their
// loops and refuses further registrations.
func TestShutdownUnregistersRegistrations(t *testing.T) {
	var unregisters atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			unregisters.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg, err := dg.Register(ctx, "sip:alice@localhost")
	require.NoError(t, err)
	require.NoError(t, reg.WaitReady(ctx))

	require.NoError(t, dg.Close(ctx))
	assert.Equal(t, int32(1), unregisters.Load(), "Close must unregister the registrations it created")
	select {
	case <-reg.Done():
	default:
		t.Fatal("Close must stop the registration loop")
	}

	// Idempotent, and registrations are refused once the agent is closed.
	require.NoError(t, dg.Close(ctx))
	assert.Equal(t, int32(1), unregisters.Load())
	_, err = dg.Register(ctx, "sip:bob@localhost")
	assert.ErrorIs(t, err, ErrAgentClosed)
	_, err = dg.Register(ctx, "sip:bob@localhost", WithRegistrationDetached())
	assert.ErrorIs(t, err, ErrAgentClosed)
}

// TestShutdownKeepsDetachedRegistrations locks WithRegistrationDetached: the
// handle keeps running after Agent.Close and its owner unregisters it.
func TestShutdownKeepsDetachedRegistrations(t *testing.T) {
	var unregisters atomic.Int32
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if h := req.GetHeader("Expires"); h != nil && h.Value() == "0" {
			unregisters.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg, err := dg.Register(ctx, "sip:alice@localhost", WithRegistrationDetached())
	require.NoError(t, err)
	require.NoError(t, reg.WaitReady(ctx))

	require.NoError(t, dg.Close(ctx))
	select {
	case <-reg.Done():
		t.Fatal("a detached registration must keep running after Agent.Close")
	default:
	}
	assert.Zero(t, unregisters.Load(), "Agent.Close must not unregister a detached registration")

	require.NoError(t, reg.Close(ctx))
	assert.Equal(t, int32(1), unregisters.Load(), "the owner unregisters the detached registration")
}

// TestRegistrationUntrackedAfterLoopExit locks that a handle whose loop ended
// on its own is no longer tracked, so the agent does not accumulate dead handles.
func TestRegistrationUntrackedAfterLoopExit(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg, err := dg.Register(ctx, "sip:alice@localhost")
	require.NoError(t, err)
	require.Error(t, reg.Wait())

	dg.registrationsMu.Lock()
	assert.Empty(t, dg.registrations)
	dg.registrationsMu.Unlock()
}

// TestIntegrationDiagoShutdownServerDialog exercises the full loop: a real
// incoming call, then the server side Close hangs the call up (BYE reaches
// the client leg), evicts the dialog and stops the listeners.
func TestIntegrationDiagoShutdownServerDialog(t *testing.T) {
	uaS, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { uaS.Close() })

	dgS, err := newAgent(withUserAgent(uaS), withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, err)
	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, dgS.ServeBackground(ctx, func(ctx context.Context, d *InboundCall) error {
		d.Trying(ctx)
		// Serve handler goroutine: return the error instead of require
		// (FailNow would abandon the handler before it signals handlerDone).
		if err := d.Answer(ctx); err != nil {
			return err
		}
		<-d.Context().Done()
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
		return nil
	}))
	serverPort := dgS.transports[0].BindPort

	uaC, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { uaC.Close() })
	dgC, err := newAgent(withUserAgent(uaC), withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, err)

	d, err := dgC.DialURI(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: serverPort}, WithDialogTransport("udp"))
	require.NoError(t, err)
	t.Cleanup(func() { d.release() })

	// Wait for the server dialog to reach Confirmed (ACK processed), so
	// Close exercises the BYE path instead of racing the 200/ACK
	// handshake.
	require.Eventually(t, func() bool {
		confirmed := false
		dgS.cache.server.rangeDialogs(ctx, func(id string, dd *InboundCall) bool {
			confirmed = dd.sess.LoadState() == sip.DialogStateConfirmed
			return false
		})
		return confirmed
	}, 5*time.Second, 10*time.Millisecond)

	serverDialogs := 0
	dgS.cache.server.rangeDialogs(ctx, func(id string, dd *InboundCall) bool {
		serverDialogs++
		return true
	})
	require.Equal(t, 1, serverDialogs, "server dialog must be tracked")

	// Server Close hangs the call up: BYE goes out, the client leg ends.
	require.NoError(t, dgS.Close(ctx))

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serve handler did not finish after Close")
	}

	select {
	case <-d.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client dialog was not terminated by server Close")
	}

	// The framework evicts server dialogs from the cache in the INVITE
	// handler's deferred cleanup, which runs after the user handler returns;
	// allow that to settle instead of racing it.
	require.Eventually(t, func() bool {
		serverDialogs := 0
		dgS.cache.server.rangeDialogs(ctx, func(id string, dd *InboundCall) bool {
			serverDialogs++
			return true
		})
		return serverDialogs == 0
	}, 2*time.Second, 10*time.Millisecond, "server cache must be drained")

	// Client side shutdown tears down its (now ended) dialog state cleanly.
	require.NoError(t, dgC.Close(ctx))
}

// TestDiagoServePartialFailureReturnsError locks that a transport bind failure
// surfaces through ServeBackground instead of hanging behind healthy
// listeners.
func TestDiagoServePartialFailureReturnsError(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	// Occupy a UDP port so the second listener fails deterministically with
	// EADDRINUSE while the first one starts fine.
	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { occupied.Close() })
	occupiedPort := occupied.LocalAddr().(*net.UDPAddr).Port

	dg, err := newAgent(withUserAgent(ua),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		withTransport(Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: occupiedPort}),
	)
	require.NoError(t, err)

	chErr := make(chan error, 1)
	go func() {
		chErr <- dg.ServeBackground(context.Background(), func(ctx context.Context, d *InboundCall) error {
			return nil
		})
	}()
	select {
	case err := <-chErr:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ServeBackground did not report the bind failure")
	}

	// The failure must stop the working listener too: Serve returning an
	// error means nothing is serving anymore, so its port is bindable again.
	port := dg.transports[0].BindPort
	require.NotZero(t, port, "working listener must have bound an ephemeral port")
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	require.NoError(t, err, "listeners must be stopped after a partial serve failure")
	conn.Close()
}
