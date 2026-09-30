// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testMediaTracer struct{}

func (testMediaTracer) RTPTraceRead(string, string, *rtp.Packet)  {}
func (testMediaTracer) RTPTraceWrite(string, string, *rtp.Packet) {}

// TestNewOptionValidation locks the AgentOption contract: options validate
// their own input and abort construction instead of failing later.
func TestNewOptionValidation(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	t.Run("nil user agent", func(t *testing.T) {
		_, err := New(WithUserAgent(nil), WithDefaultTransport())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "user agent is nil")
	})

	t.Run("empty transport network", func(t *testing.T) {
		_, err := New(WithUserAgent(ua), WithTransport(Transport{}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network is empty")
	})

	t.Run("media config without codecs", func(t *testing.T) {
		_, err := New(WithUserAgent(ua), WithDefaultTransport(), WithMediaConfig(MediaConfig{}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no codecs")
	})

	t.Run("media config inverted port range", func(t *testing.T) {
		_, err := New(WithUserAgent(ua), WithDefaultTransport(), WithMediaConfig(MediaConfig{
			Codecs:  []media.Codec{media.CodecAudioUlaw},
			Network: NetworkConfig{RTPPortStart: 20000, RTPPortEnd: 19000},
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inverted")
	})

	t.Run("nil logger", func(t *testing.T) {
		_, err := New(WithUserAgent(ua), WithDefaultTransport(), WithLogger(nil))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "logger is nil")
	})

	t.Run("nil http client", func(t *testing.T) {
		_, err := New(WithUserAgent(ua), WithDefaultTransport(), WithHTTPClient(nil))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client is nil")
	})

	t.Run("nil options are skipped", func(t *testing.T) {
		dg, err := New(WithUserAgent(ua), WithDefaultTransport(), nil, WithMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
		}))
		require.NoError(t, err)
		require.NotNil(t, dg)
	})
}

// TestNewPrivateUserAgent locks the zero-setup path: without WithUserAgent,
// New creates a private UserAgent that Close owns.
func TestNewPrivateUserAgent(t *testing.T) {
	dg, err := New(WithDefaultTransport(), WithUserAgentName("diago-test"))
	require.NoError(t, err)
	require.NotNil(t, dg.UA())
	require.True(t, dg.ownedUA, "private UserAgent must be owned by the agent")
	require.NoError(t, dg.Close(context.Background()))
}

// TestIntegrationPrivateUserAgentsCall covers the zero-setup path end to end:
// both legs create their own UserAgent and the call establishes and ends.
func TestIntegrationPrivateUserAgentsCall(t *testing.T) {
	skipShort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	server, err := New(WithTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)
	t.Cleanup(func() { server.Close(context.Background()) })
	require.NoError(t, server.ServeBackground(ctx, func(ctx context.Context, call *InboundCall) error {
		if err := call.Answer(ctx); err != nil {
			return err
		}
		<-call.Context().Done()
		return nil
	}))

	client, err := New(WithTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)
	t.Cleanup(func() { client.Close(context.Background()) })
	require.NoError(t, client.ServeBackground(ctx, nil))

	port := server.transports[0].BindPort
	require.NotZero(t, port)

	call, err := client.Dial(ctx, "sip:alice@127.0.0.1:"+strconv.Itoa(port))
	require.NoError(t, err)
	require.NoError(t, call.Hangup(ctx))
	require.NoError(t, call.Close())
}

// TestNewCallerOwnedUserAgent locks the shared-UA path: WithUserAgent keeps
// ownership with the caller and Close does not close it.
func TestNewCallerOwnedUserAgent(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := New(WithUserAgent(ua), WithDefaultTransport())
	require.NoError(t, err)
	require.False(t, dg.ownedUA)
	require.Same(t, ua, dg.UA())
	require.Same(t, ua, dg.Server().UserAgent)

	require.NoError(t, dg.Close(context.Background()))
	// The caller still owns the UA: closing it must not have happened already
	// in a way that breaks this call.
	require.NoError(t, ua.Close())
}

// TestAgentHandle locks the extra-method registration: before Serve it
// registers (wrapped by the middleware), while serving it is refused, and a
// nil handler is rejected.
func TestAgentHandle(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := New(WithUserAgent(ua), WithTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
	}))
	require.NoError(t, err)

	require.Error(t, dg.Handle(sip.MESSAGE, nil))
	require.NoError(t, dg.Handle(sip.MESSAGE, func(req *Request, tx ServerTransaction) error { return nil }))

	// Methods managed by the agent cannot be replaced: silently overriding
	// them would bypass dialog and media handling.
	err = dg.Handle(sip.INVITE, func(req *Request, tx ServerTransaction) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "managed by the agent")

	require.NoError(t, dg.ServeBackground(context.Background(), nil))
	t.Cleanup(func() { dg.Close(context.Background()) })

	err = dg.Handle(sip.SUBSCRIBE, func(req *Request, tx ServerTransaction) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before Serve")
}

// TestMediaConfigPropagation locks the per-instance debug/tracer/HTTP client
// plumbing: the values reach the media session and the playback created on a
// dialog.
func TestMediaConfigPropagation(t *testing.T) {
	httpClient := &http.Client{Timeout: time.Second}
	tracer := testMediaTracer{}

	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	}, WithMediaConfig(MediaConfig{
		Codecs:     []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		Debug:      true,
		Tracer:     tracer,
		HTTPClient: httpClient,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d, err := dg.DialURI(ctx, sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.Close()

	require.Same(t, httpClient, d.callMedia.httpClient)

	ms := d.callMedia.mediaSession
	require.NotNil(t, ms)
	assert.True(t, ms.RTPDebug)
	assert.True(t, ms.RTCPDebug)
	assert.Equal(t, tracer, ms.Tracer)

	pb, err := d.callMedia.StartPlayback()
	require.NoError(t, err)
	require.Same(t, httpClient, pb.httpClient)
}

// TestWithHTTPClientFallback locks that WithHTTPClient fills
// MediaConfig.HTTPClient when the media config does not carry one, regardless
// of option order.
func TestWithHTTPClientFallback(t *testing.T) {
	httpClient := &http.Client{Timeout: time.Second}

	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := New(WithUserAgent(ua),
		WithDefaultTransport(),
		WithMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}),
		WithHTTPClient(httpClient),
	)
	require.NoError(t, err)
	require.Same(t, httpClient, dg.mediaConf.HTTPClient)
}
