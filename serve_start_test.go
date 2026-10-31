// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func startTestAgent(t *testing.T) *Agent {
	t.Helper()
	dg, err := newAgent(withTransport(Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: 0,
	}))
	require.NoError(t, err)
	return dg
}

func TestStartHandleClose(t *testing.T) {
	dg := startTestAgent(t)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, dg.Close(ctx))
	}()

	h, err := dg.Start(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, h)
	port := agentPort(t, dg)

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.Close(sctx))

	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("ServeHandle.Done did not close after Close")
	}
	require.NoError(t, h.Err())

	// The listener socket must be released after the handle is closed.
	conn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	_ = conn.Close()
}

func TestStartReportsStartupFailure(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	dg, err := newAgent(withTransport(Transport{
		Network:  "udp",
		BindHost: "127.0.0.1",
		BindPort: port,
	}))
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, dg.Close(ctx))
	}()

	h, err := dg.Start(context.Background(), nil)
	require.Error(t, err)
	require.Nil(t, h)
}
