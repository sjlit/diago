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
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shutdownTestSDP() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
}

func cacheCountClientDialogs(dg *Diago) int {
	n := 0
	dg.cache.client.DialogRange(context.Background(), func(id string, d *DialogClientSession) bool {
		n++
		return true
	})
	return n
}

// TestDiagoShutdownTearsDownClientDialogs covers Shutdown phase 1 without real
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

	_, err := dg.Invite(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	require.Equal(t, 1, cacheCountClientDialogs(dg), "confirmed dialog must be tracked")

	require.NoError(t, dg.Shutdown(ctx))
	assert.Equal(t, int32(1), byeCount.Load(), "Shutdown must send BYE to confirmed dialogs")
	assert.Equal(t, 0, cacheCountClientDialogs(dg), "Shutdown must evict dialogs from the cache")

	// Idempotent.
	require.NoError(t, dg.Shutdown(ctx))

	// Serving after Shutdown is refused.
	err = dg.serve(ctx, func(d *DialogServerSession) {}, func() {})
	require.Error(t, err)
	assert.ErrorContains(t, err, "shut down")

}

// TestDiagoShutdownStopsListeners covers phase 2 with real UDP listeners.
func TestDiagoShutdownStopsListeners(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, dg.ServeBackground(ctx, func(d *DialogServerSession) {}))
	require.NotZero(t, dg.transports[0].BindPort, "listener must have bound an ephemeral port")
	assert.True(t, dg.serving.Load())

	require.NoError(t, dg.Shutdown(ctx))
	assert.False(t, dg.serving.Load(), "listeners must be stopped after Shutdown")

	// Already-serving must be rejected while up.
	dg2 := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, dg2.ServeBackground(ctx, func(d *DialogServerSession) {}))
	t.Cleanup(func() { dg2.Shutdown(context.Background()) })
	err = dg2.serve(ctx, func(d *DialogServerSession) {}, func() {})
	require.Error(t, err)
	assert.ErrorContains(t, err, "already serving")
}

// TestIntegrationDiagoShutdownServerDialog exercises the full loop: a real
// incoming call, then the server side Shutdown hangs the call up (BYE reaches
// the client leg), evicts the dialog and stops the listeners.
func TestIntegrationDiagoShutdownServerDialog(t *testing.T) {
	uaS, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { uaS.Close() })

	dgS := NewDiago(uaS, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	handlerDone := make(chan struct{})
	var handlerOnce atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, dgS.ServeBackground(ctx, func(d *DialogServerSession) {
		d.Trying()
		require.NoError(t, d.Answer())
		<-d.Context().Done()
		if handlerOnce.CompareAndSwap(false, true) {
			close(handlerDone)
		}
	}))
	serverPort := dgS.transports[0].BindPort

	uaC, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { uaC.Close() })
	dgC := NewDiago(uaC, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))

	d, err := dgC.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: serverPort}, WithDialogTransport("udp"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	// Wait for the server dialog to reach Confirmed (ACK processed), so
	// Shutdown exercises the BYE path instead of racing the 200/ACK
	// handshake.
	require.Eventually(t, func() bool {
		confirmed := false
		dgS.cache.server.DialogRange(ctx, func(id string, dd *DialogServerSession) bool {
			confirmed = dd.LoadState() == sip.DialogStateConfirmed
			return false
		})
		return confirmed
	}, 5*time.Second, 10*time.Millisecond)

	serverDialogs := 0
	dgS.cache.server.DialogRange(ctx, func(id string, dd *DialogServerSession) bool {
		serverDialogs++
		return true
	})
	require.Equal(t, 1, serverDialogs, "server dialog must be tracked")

	// Server Shutdown hangs the call up: BYE goes out, the client leg ends.
	require.NoError(t, dgS.Shutdown(ctx))

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serve handler did not finish after Shutdown")
	}

	select {
	case <-d.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client dialog was not terminated by server Shutdown")
	}

	serverDialogs = 0
	dgS.cache.server.DialogRange(ctx, func(id string, dd *DialogServerSession) bool {
		serverDialogs++
		return true
	})
	assert.Equal(t, 0, serverDialogs, "server cache must be drained")

	// Client side shutdown tears down its (now ended) dialog state cleanly.
	require.NoError(t, dgC.Shutdown(ctx))
}

// TestDiagoServePartialFailureReturnsError locks that a transport bind failure
// surfaces through ServeBackground instead of hanging behind healthy
// listeners.
func TestDiagoServePartialFailureReturnsError(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	// The second transport uses an unsupported transport name, so its
	// ListenAndServe fails deterministically (sip.ErrTransportNotSuported).
	dg := NewDiago(ua,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		WithTransport(Transport{Transport: "bogus", BindHost: "127.0.0.1", BindPort: 0}),
	)

	chErr := make(chan error, 1)
	go func() {
		chErr <- dg.ServeBackground(context.Background(), func(d *DialogServerSession) {})
	}()
	select {
	case err := <-chErr:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ServeBackground did not report the bind failure")
	}
}
