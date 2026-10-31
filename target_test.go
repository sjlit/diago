// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseTargetURI locks the string entry parsing: valid SIP URIs pass
// through (including uri parameters), invalid or empty targets fail with
// ErrInvalidTarget.
func TestParseTargetURI(t *testing.T) {
	t.Run("user and host", func(t *testing.T) {
		uri, err := parseTargetURI("sip:alice@example.com")
		require.NoError(t, err)
		assert.Equal(t, "alice", uri.User)
		assert.Equal(t, "example.com", uri.Host)
	})

	t.Run("port and transport parameter", func(t *testing.T) {
		uri, err := parseTargetURI("sip:bob@127.0.0.1:5060;transport=tcp")
		require.NoError(t, err)
		assert.Equal(t, 5060, uri.Port)
		transport, ok := uri.UriParams.Get("transport")
		require.True(t, ok)
		assert.Equal(t, "tcp", transport)
	})

	t.Run("trimmed target", func(t *testing.T) {
		uri, err := parseTargetURI("  sip:alice@example.com  ")
		require.NoError(t, err)
		assert.Equal(t, "example.com", uri.Host)
	})

	t.Run("empty target", func(t *testing.T) {
		_, err := parseTargetURI("   ")
		require.ErrorIs(t, err, ErrInvalidTarget)
	})

	t.Run("malformed target", func(t *testing.T) {
		_, err := parseTargetURI("sip:")
		require.ErrorIs(t, err, ErrInvalidTarget)
	})
}

// TestDialURIKeepsCallerURIParams locks that consuming the "transport" URI
// parameter never mutates the caller's sip.Uri (the params are a shared
// slice; the library must work on a copy).
func TestDialURIKeepsCallerURIParams(t *testing.T) {
	// The fake requester fails the call before any request is sent only when
	// no transport can be found; use a UDP-only agent so "tcp" fails in
	// newDialogWithParams after the parameter was consumed.
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	uri := sip.Uri{User: "alice", Host: "localhost"}
	uri.UriParams = sip.NewParams()
	uri.UriParams.Add("transport", "tcp")

	_, err := dg.DialURI(ctx, uri)
	require.Error(t, err, "tcp transport is not configured")
	assert.Contains(t, err.Error(), "tcp")

	transport, ok := uri.UriParams.Get("transport")
	assert.True(t, ok, "caller URI params must not be consumed")
	assert.Equal(t, "tcp", transport)
	assert.Len(t, uri.UriParams, 1, "caller URI params must not be mutated")
}

// TestDiagoDialStringTarget covers the primary string entry end to end: the
// target is parsed by the library and the call is established.
func TestDiagoDialStringTarget(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d, err := dg.Dial(ctx, "sip:alice@localhost")
	require.NoError(t, err)
	require.NotNil(t, d)
	require.NoError(t, d.Hangup(ctx))
	require.NoError(t, d.release())

	_, err = dg.Dial(ctx, "sip:")
	require.ErrorIs(t, err, ErrInvalidTarget)
}
