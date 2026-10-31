// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaOptionsApply(t *testing.T) {
	base := MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Network: NetworkConfig{
			BindIP:     net.IPv4(10, 0, 0, 1),
			ExternalIP: net.IPv4(1, 1, 1, 1),
		},
	}
	nat := RTPNATLearnSource
	tone := audio.Tone{Segments: []audio.ToneSegment{{Freqs: []float64{440}, On: time.Second}}}
	p := &signalParams{}
	require.NoError(t, (MediaOptions{
		Codecs:      []media.Codec{media.CodecAudioAlaw},
		RTPNAT:      &nat,
		BindIP:      net.IPv4(10, 0, 0, 2),
		ExternalIP:  net.IPv4(2, 2, 2, 2),
		SessionName: "Override",
		MusicOnHold: &tone,
	}).apply(p))

	conf := signalMediaConfig(base, p)
	assert.Equal(t, []media.Codec{media.CodecAudioAlaw}, conf.Codecs)
	assert.Equal(t, RTPNATLearnSource, conf.Network.RTPNAT)
	assert.Equal(t, "10.0.0.2", conf.Network.BindIP.String())
	assert.Equal(t, "2.2.2.2", conf.Network.ExternalIP.String())
	assert.Equal(t, "Override", conf.SDP.SessionName)
	assert.Equal(t, tone, conf.MusicOnHold)

	// Base config must not be mutated.
	assert.Equal(t, "1.1.1.1", base.Network.ExternalIP.String())
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, base.Codecs)
}

func TestMediaOptionsValidation(t *testing.T) {
	nat := RTPNATMode(99)
	cases := []struct {
		name string
		opts MediaOptions
	}{
		{"empty codecs", MediaOptions{Codecs: []media.Codec{}}},
		{"invalid rtp nat", MediaOptions{RTPNAT: &nat}},
		{"empty dtls", MediaOptions{DTLS: &media.DTLSConfig{}}},
		{"session name line break", MediaOptions{SessionName: "x\nm=audio 0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.opts.apply(&signalParams{}), ErrInvalidOption)
		})
	}

	sess := &media.MediaSession{}
	p := &signalParams{}
	require.NoError(t, (MediaOptions{Session: sess}).apply(p))
	assert.Same(t, sess, p.Media.MediaSession)
}

func TestRegisterOptionsValidation(t *testing.T) {
	_, err := (RegisterOptions{Expiry: -time.Second}).signalParams()
	require.ErrorIs(t, err, ErrInvalidOption)
	_, err = (RegisterOptions{RetryInterval: -time.Second}).signalParams()
	require.ErrorIs(t, err, ErrInvalidOption)
	_, err = (RegisterOptions{AllowHeaders: []string{""}}).signalParams()
	require.ErrorIs(t, err, ErrInvalidOption)
}

func TestBuildReInviteRequestWithOptions(t *testing.T) {
	contact := &ContactHeader{Address: URI{Scheme: "sip", User: "override", Host: "127.0.0.1"}}
	params, err := messageSignalParams(
		[]Header{sip.NewHeader("X-Test", "1")},
		contact,
		[]byte("custom-sdp"),
		func(req *Request) error {
			req.AppendHeader(sip.NewHeader("X-Mutated", "yes"))
			return nil
		},
		nil,
	)
	require.NoError(t, err)

	req := sip.NewRequest(sip.INVITE, URI{User: "alice", Host: "127.0.0.1"})
	require.NoError(t, buildReInviteRequest(req, []byte("base-sdp"), nil, params))

	assert.Equal(t, "custom-sdp", string(req.Body()))
	assert.Equal(t, "1", req.GetHeader("X-Test").Value())
	assert.Equal(t, "yes", req.GetHeader("X-Mutated").Value())
	require.NotNil(t, req.Contact())
	assert.Equal(t, "override", req.Contact().Address.User)
}
