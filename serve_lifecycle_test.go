// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/stretchr/testify/require"
)

// freeUDPPort returns a currently free UDP port on the loopback interface.
// Tests that must pin a port (for rebinding assertions) use it instead of a
// shared fixed value.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// Regression: canceling the caller context during the listener startup window
// used to be swallowed by sipgo's ctx-watcher (no socket to close yet). Serve
// then never returned, Close timed out, and the UDP port stayed bound for
// the process lifetime.
func TestServeCallerContextCancelDuringStartup(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg, err := New(WithUserAgent(ua), WithTransport(Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: port,
	}))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- dg.Serve(ctx, nil) }()
	// Cancel inside the listener startup window.
	cancel()

	select {
	case <-done:
		// Serve may report the listener's context error; the contract that
		// matters is that it returns and nothing keeps serving.
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the caller context was canceled")
	}

	sctx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	require.NoError(t, dg.Close(sctx))

	conn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "listener socket must be released after cancel and shutdown")
	_ = conn.Close()
}
