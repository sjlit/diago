// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOptionScopeRejectsUnsupported locks the scope table: options outside the
// API's declared set fail with *UnsupportedOptionError instead of being
// silently ignored.
func TestOptionScopeRejectsUnsupported(t *testing.T) {
	t.Run("WithBody on Trying", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Trying", scopeTrying)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, "Trying", unsupported.Method)
		assert.Equal(t, []string{"WithBody"}, unsupported.Options)
	})

	t.Run("WithCodecs on ServerHangup", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithCodecs(media.CodecAudioAlaw)}, "Hangup", scopeServerHangup)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithCodecs"}, unsupported.Options)
	})

	t.Run("WithRegisterExpiry on Qualify", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithRegisterExpiry(time.Second)}, "Qualify", scopeRegisterAttempt)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithRegisterExpiry"}, unsupported.Options)
	})

	t.Run("WithDialogTransport on Invite", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithDialogTransport("udp")}, "Invite", scopeClientInvite)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithDialogTransport"}, unsupported.Options)
	})

	t.Run("WithAuthCredentials on Answer", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithAuthCredentials("u", "p")}, "Answer", scopeAnswer)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithAuthCredentials"}, unsupported.Options)
	})

	t.Run("WithMusicOnHold on Unhold", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithMusicOnHold(audio.Tone{})}, "Unhold", scopeUnhold)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithMusicOnHold"}, unsupported.Options)
	})

	t.Run("WithBody on Ack", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Ack", scopeClientAck)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithBody"}, unsupported.Options)
	})

	t.Run("WithBody on RegisterAttempt", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Register", scopeRegisterAttempt)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithBody"}, unsupported.Options)
	})

	t.Run("WithBody on Refer", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Refer", scopeRefer)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithBody"}, unsupported.Options)
	})

	t.Run("WithBody allowed on ReInvite and Hold", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "ReInvite", scopeReInvite)
		require.NoError(t, err)
		_, err = newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Hold", scopeHold)
		require.NoError(t, err)
	})

	t.Run("DeduplicatesAndSortsDiagnostics", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{
			WithBody([]byte("v=0")),
			WithBody([]byte("v=1")),
			WithCodecs(media.CodecAudioAlaw),
		}, "Trying", scopeTrying)
		var unsupported *UnsupportedOptionError
		require.ErrorAs(t, err, &unsupported)
		assert.Equal(t, []string{"WithBody", "WithCodecs"}, unsupported.Options)
		assert.Equal(t, "Trying: unsupported options WithBody, WithCodecs", unsupported.Error())
	})

	t.Run("SingleOptionMessage", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{WithBody([]byte("v=0"))}, "Ringing", scopeRinging)
		require.EqualError(t, err, "Ringing: unsupported option WithBody")
	})

	t.Run("AllowedOptionsPass", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{
			WithHeader("X-Test", "1"),
			WithContact(&sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1"}}),
			WithResponseMutator(func(res *sip.Response) error { return nil }),
		}, "Trying", scopeTrying)
		require.NoError(t, err)
	})

	t.Run("DialogTransportAllowedOnDial", func(t *testing.T) {
		_, err := newSignalParams([]SignalOption{
			WithDialogTransport("udp"),
			WithDialogTransportID("tr-1"),
		}, "Dial", scopeDial)
		require.NoError(t, err)
	})
}

// TestEveryOptionMarksScope guards the mark() calls: an option that forgets to
// mark its scope would silently pass every API's validation.
func TestEveryOptionMarksScope(t *testing.T) {
	options := map[string]SignalOption{
		"WithHeaders":               WithHeaders(sip.NewHeader("X", "1")),
		"WithHeader":                WithHeader("X", "1"),
		"WithContact":               WithContact(&sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1"}}),
		"WithBody":                  WithBody([]byte("v=0")),
		"WithCodecs":                WithCodecs(media.CodecAudioAlaw),
		"WithRTPNAT":                WithRTPNAT(1),
		"WithMediaBindIP":           WithMediaBindIP(net.ParseIP("127.0.0.1")),
		"WithMediaExternalIP":       WithMediaExternalIP(net.ParseIP("127.0.0.1")),
		"WithMediaDTLS":             WithMediaDTLS(media.DTLSConfig{ServerName: "sip.example"}),
		"WithSDPSessionName":        WithSDPSessionName("Name"),
		"WithMediaSession":          WithMediaSession(&media.MediaSession{}),
		"WithMusicOnHold":           WithMusicOnHold(audio.Tone{}),
		"WithOnResponse":            WithOnResponse(func(res *sip.Response) error { return nil }),
		"WithOnMediaUpdate":         WithOnMediaUpdate(func(d *DialogMedia) {}),
		"WithOnRefer":               WithOnRefer(func(d *DialogClientSession) error { return nil }),
		"WithOnReferNotify":         WithOnReferNotify(func(statusCode int) {}),
		"WithRequestMutator":        WithRequestMutator(func(req *sip.Request) error { return nil }),
		"WithResponseMutator":       WithResponseMutator(func(res *sip.Response) error { return nil }),
		"WithDialogTransport":       WithDialogTransport("udp"),
		"WithDialogTransportID":     WithDialogTransportID("tr-1"),
		"WithOriginator":            WithOriginator(newBridgeTestDialog("o")),
		"WithAuthCredentials":       WithAuthCredentials("u", "p"),
		"WithEarlyMediaDetect":      WithEarlyMediaDetect(),
		"WithRegisterExpiry":        WithRegisterExpiry(time.Second),
		"WithRegisterRetryInterval": WithRegisterRetryInterval(time.Second),
		"WithRegisterAllowHeaders":  WithRegisterAllowHeaders("INVITE"),
		"WithRegisterProxyHost":     WithRegisterProxyHost("127.0.0.1:5060"),
		"WithOnRegistered":          WithOnRegistered(func() {}),
	}

	for name, opt := range options {
		t.Run(name, func(t *testing.T) {
			p, err := newSignalParams([]SignalOption{opt}, "Test", scopeTestAll)
			require.NoError(t, err)
			require.Len(t, p.touched, 1, "option must mark exactly one scope")
			assert.Equal(t, name, p.touched[0].name)
			assert.NotZero(t, p.touched[0].scope)
		})
	}
}

// TestIntegrationUnsupportedOptionRejected verifies the scope error surfaces
// through a real call: the handler's Trying(ctx, WithBody) fails and returning
// that error lets the framework answer 500.
func TestIntegrationUnsupportedOptionRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handlerErr := make(chan error, 1)

	uaSrv, _ := sipgo.NewUA()
	defer uaSrv.Close()
	dgSrv := NewDiago(uaSrv, WithTransport(Transport{
		Transport: "udp", BindHost: "127.0.0.1", BindPort: 15260,
	}))
	err := dgSrv.ServeBackground(ctx, func(ctx context.Context, d *DialogServerSession) error {
		e := d.Trying(ctx, WithBody([]byte("v=0")))
		handlerErr <- e
		return e
	})
	require.NoError(t, err)

	uaCli, _ := sipgo.NewUA()
	defer uaCli.Close()
	dgCli := NewDiago(uaCli, WithTransport(Transport{
		Transport: "udp", BindHost: "127.0.0.1", BindPort: 15261,
	}))

	_, err = dgCli.Dial(ctx, sip.Uri{User: "alice", Host: "127.0.0.1", Port: 15260})
	require.Error(t, err, "server must answer the unsupported option with 500")

	var unsupported *UnsupportedOptionError
	require.ErrorAs(t, <-handlerErr, &unsupported)
	assert.Equal(t, "Trying", unsupported.Method)
	assert.Equal(t, []string{"WithBody"}, unsupported.Options)

	var dialogErrPtr *sipgo.ErrDialogResponse
	var dialogErrVal sipgo.ErrDialogResponse
	switch {
	case errors.As(err, &dialogErrPtr):
		assert.Equal(t, sip.StatusInternalServerError, dialogErrPtr.Res.StatusCode)
	case errors.As(err, &dialogErrVal):
		assert.Equal(t, sip.StatusInternalServerError, dialogErrVal.Res.StatusCode)
	default:
		t.Fatalf("client must observe the mapped 500, got %v", err)
	}
}

// TestSignalOptionValidation covers the uniform constructor validation.
func TestSignalOptionValidation(t *testing.T) {
	cases := []struct {
		name string
		opt  SignalOption
	}{
		{"WithHeaders rejects nil header", WithHeaders(nil)},
		{"WithHeader rejects empty name", WithHeader("", "v")},
		{"WithContact rejects nil", WithContact(nil)},
		{"WithBody rejects nil", WithBody(nil)},
		{"WithCodecs rejects empty list", WithCodecs()},
		{"WithRTPNAT rejects out of range", WithRTPNAT(2)},
		{"WithMediaBindIP rejects nil", WithMediaBindIP(nil)},
		{"WithMediaExternalIP rejects nil", WithMediaExternalIP(nil)},
		{"WithMediaDTLS rejects empty config", WithMediaDTLS(media.DTLSConfig{})},
		{"WithSDPSessionName rejects empty", WithSDPSessionName("")},
		{"WithMediaSession rejects nil", WithMediaSession(nil)},
		{"WithOnResponse rejects nil", WithOnResponse(nil)},
		{"WithOnMediaUpdate rejects nil", WithOnMediaUpdate(nil)},
		{"WithOnRefer rejects nil", WithOnRefer(nil)},
		{"WithOnReferNotify rejects nil", WithOnReferNotify(nil)},
		{"WithRequestMutator rejects nil", WithRequestMutator(nil)},
		{"WithResponseMutator rejects nil", WithResponseMutator(nil)},
		{"WithDialogTransport rejects empty", WithDialogTransport("")},
		{"WithDialogTransportID rejects empty", WithDialogTransportID("")},
		{"WithOriginator rejects nil", WithOriginator(nil)},
		{"WithAuthCredentials rejects empty", WithAuthCredentials("", "")},
		{"WithRegisterExpiry rejects negative", WithRegisterExpiry(-time.Second)},
		{"WithRegisterRetryInterval rejects negative", WithRegisterRetryInterval(-time.Second)},
		{"WithRegisterAllowHeaders rejects empty list", WithRegisterAllowHeaders()},
		{"WithRegisterAllowHeaders rejects empty value", WithRegisterAllowHeaders("")},
		{"WithRegisterProxyHost rejects empty", WithRegisterProxyHost("")},
		{"WithOnRegistered rejects nil", WithOnRegistered(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSignalParams([]SignalOption{tc.opt}, "Test", scopeTestAll)
			require.Error(t, err)
		})
	}
}
