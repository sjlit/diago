// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/require"
)

func testConfig() Config {
	return Config{
		Transports: []Transport{{
			Network:  "udp",
			BindHost: "127.0.0.1",
			BindPort: 0,
		}},
	}
}

func closeTestAgent(t *testing.T, dg *Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, dg.Close(ctx))
}

func TestNewConfig(t *testing.T) {
	dg, err := New(testConfig())
	require.NoError(t, err)
	defer closeTestAgent(t, dg)

	require.NotNil(t, dg.UA())
	require.NotNil(t, dg.Server())
	require.True(t, dg.ownedUA)
	require.Len(t, dg.mediaConf.Codecs, 3)
}

func TestNewConfigNoTransport(t *testing.T) {
	_, err := New(Config{})
	require.ErrorIs(t, err, ErrNoTransport)
}

func TestNewConfigCustomMedia(t *testing.T) {
	conf := testConfig()
	conf.Media = MediaConfig{
		Codecs: []media.Codec{media.CodecAudioAlaw},
	}
	dg, err := New(conf)
	require.NoError(t, err)
	defer closeTestAgent(t, dg)

	require.Equal(t, []media.Codec{media.CodecAudioAlaw}, dg.mediaConf.Codecs)
}

func TestNewConfigMediaProfile(t *testing.T) {
	conf := testConfig()
	conf.MediaProfiles = map[string]MediaConfig{
		"profile": {Codecs: []media.Codec{media.CodecAudioAlaw}},
	}
	conf.Transports[0].Profile = "profile"
	dg, err := New(conf)
	require.NoError(t, err)
	defer closeTestAgent(t, dg)

	require.Contains(t, dg.mediaProfiles, "profile")

	conf.Transports[0].Profile = "missing"
	_, err = New(conf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")
}

func TestNewConfigCallerOwnedUserAgent(t *testing.T) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("caller"))
	require.NoError(t, err)
	defer ua.Close()

	conf := testConfig()
	conf.UserAgent = ua
	dg, err := New(conf)
	require.NoError(t, err)
	defer closeTestAgent(t, dg)

	require.Same(t, ua, dg.UA())
	require.False(t, dg.ownedUA)
}

func TestNewConfigPassThrough(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	client := &http.Client{}
	middleware := func(next RequestHandler) RequestHandler { return next }

	conf := testConfig()
	conf.Logger = logger
	conf.HTTPClient = client
	conf.Middleware = middleware
	conf.UserAgentName = "config-name"
	dg, err := New(conf)
	require.NoError(t, err)
	defer closeTestAgent(t, dg)

	require.Same(t, logger, dg.log)
	require.Same(t, client, dg.httpClient)
	require.NotNil(t, dg.serverMiddleware)
	require.Equal(t, "config-name", dg.UA().Name())
}
