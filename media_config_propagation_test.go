// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiagoMediaConfigForTransport locks that the per-dialog media config
// carries ALL global MediaConfig fields. Regression: RTPNAT and SecureRTPAlg
// were silently dropped when building the per-dialog config.
func TestDiagoMediaConfigForTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg := NewDiago(ua, WithMediaConfig(MediaConfig{
		Codecs:                   []media.Codec{media.CodecAudioUlaw},
		RTPNAT:                   media.RTPNATSymetric,
		SecureRTPAlg:             1,
		RTPPortStart:             16000,
		RTPPortEnd:               16020,
		SDPCodecPreferLocalOrder: 1,
	}))

	conf := dg.mediaConfigForTransport(&Transport{Transport: "udp", MediaSRTP: 1})
	assert.Equal(t, media.RTPNATSymetric, conf.RTPNAT, "global RTPNAT must not be dropped")
	assert.Equal(t, uint16(1), conf.SecureRTPAlg, "global SecureRTPAlg must not be dropped")
	assert.Equal(t, 16000, conf.RTPPortStart)
	assert.Equal(t, 16020, conf.RTPPortEnd)
	assert.Equal(t, 1, conf.SDPCodecPreferLocalOrder)
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, conf.Codecs)
	assert.Equal(t, 1, conf.SecureRTP, "transport overlay must win")
}

// TestDiagoMediaConfigAppliesToSession locks that global MediaConfig settings
// reach the created media session end to end.
func TestDiagoMediaConfigAppliesToSession(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	}, WithMediaConfig(MediaConfig{
		Codecs:       []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		RTPNAT:       media.RTPNATSymetric,
		RTPPortStart: 18000,
		RTPPortEnd:   18020,
	}))

	d, err := dg.Invite(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.Close()

	ms := d.DialogMedia.mediaSession
	require.NotNil(t, ms)
	assert.Equal(t, media.RTPNATSymetric, ms.RTPNAT)
	port := ms.Laddr.Port
	assert.GreaterOrEqual(t, port, 18000, "media must bind inside the configured port range")
	assert.Less(t, port, 18020)
}
