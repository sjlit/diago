// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/sjlit/diago/v2/media"
	"github.com/sjlit/diago/v2/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiagoMediaConfigForTransport locks that the per-dialog media config
// carries ALL global MediaConfig fields. Regression: RTPNAT and SRTP alg
// were silently dropped when building the per-dialog config.
func TestDiagoMediaConfigForTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := NewDiago(ua, WithMediaConfig(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Network: NetworkConfig{
			RTPNAT:       media.RTPNATSymetric,
			RTPPortStart: 16000,
			RTPPortEnd:   16020,
		},
		SRTP: SRTPConfig{Alg: 1},
		SDP:  SDPConfig{CodecPreferLocalOrder: 1},
	}), WithDefaultTransport())
	require.NoError(t, err)

	conf := dg.mediaConfigForTransport(&Transport{
		Transport: "udp",
		Media:     TransportMediaConfig{SRTP: SRTPConfig{Mode: SecureRTPSDES}},
	})
	assert.Equal(t, media.RTPNATSymetric, int(conf.Network.RTPNAT), "global RTPNAT must not be dropped")
	assert.Equal(t, SRTPAlg(1), conf.SRTP.Alg, "global SRTP alg must not be dropped")
	assert.Equal(t, 16000, conf.Network.RTPPortStart)
	assert.Equal(t, 16020, conf.Network.RTPPortEnd)
	assert.Equal(t, 1, conf.SDP.CodecPreferLocalOrder)
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, conf.Codecs)
	assert.Equal(t, SecureRTPSDES, conf.SRTP.Mode, "transport overlay must win")
}

// TestDiagoMediaConfigTransportZeroInherits locks the merge rule: a transport
// without media overrides keeps the global values, including the ones the v1
// overlay used to clobber with zero values.
func TestDiagoMediaConfigTransportZeroInherits(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	global := MediaConfig{
		Network: NetworkConfig{
			BindIP:     net.IPv4(10, 0, 0, 5),
			ExternalIP: net.IPv4(203, 0, 113, 5),
			RTPNAT:     RTPNATLearnSource,
		},
		SRTP: SRTPConfig{Mode: SecureRTPSDES, Alg: 2},
	}
	dg, err := NewDiago(ua, WithMediaConfig(global), WithDefaultTransport())
	require.NoError(t, err)

	conf := dg.mediaConfigForTransport(&Transport{Transport: "udp"})
	assert.Equal(t, SecureRTPSDES, conf.SRTP.Mode, "zero transport mode must inherit the global mode")
	assert.Equal(t, SRTPAlg(2), conf.SRTP.Alg)
	assert.Equal(t, net.IPv4(10, 0, 0, 5), conf.Network.BindIP)
	assert.Equal(t, net.IPv4(203, 0, 113, 5), conf.Network.ExternalIP)
	assert.Equal(t, RTPNATLearnSource, conf.Network.RTPNAT)

	// A concrete BindHost pins the media IP even over an explicit global one.
	conf = dg.mediaConfigForTransport(&Transport{Transport: "udp", BindHost: "192.0.2.7"})
	assert.Equal(t, net.IPv4(192, 0, 2, 7), conf.Network.BindIP)

	// Explicit transport overrides beat both the global config and BindHost.
	conf = dg.mediaConfigForTransport(&Transport{
		Transport: "udp",
		BindHost:  "192.0.2.7",
		Media: TransportMediaConfig{Network: NetworkConfig{
			BindIP:     net.IPv4(10, 0, 0, 9),
			ExternalIP: net.IPv4(203, 0, 113, 9),
		}},
	})
	assert.Equal(t, net.IPv4(10, 0, 0, 9), conf.Network.BindIP)
	assert.Equal(t, net.IPv4(203, 0, 113, 9), conf.Network.ExternalIP)
}

// TestDiagoMediaConfigAppliesToSession locks that global MediaConfig settings
// reach the created media session end to end.
func TestDiagoMediaConfigAppliesToSession(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	}, WithMediaConfig(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		Network: NetworkConfig{
			RTPNAT:       media.RTPNATSymetric,
			RTPPortStart: 18000,
			RTPPortEnd:   18020,
		},
	}))

	d, err := dg.Dial(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.Close()

	ms := d.dialogMedia.mediaSession
	require.NotNil(t, ms)
	assert.Equal(t, media.RTPNATSymetric, ms.RTPNAT)
	port := ms.Laddr.Port
	assert.GreaterOrEqual(t, port, 18000, "media must bind inside the configured port range")
	assert.Less(t, port, 18020)
}

// TestDiagoMediaConfigSDPSessionNamePropagation locks that MediaConfig.SDP.SessionName
// flows into the MediaSession and shows up in the local SDP "s=" line. Default
// is preserved when the field is empty.
func TestDiagoMediaConfigSDPSessionNamePropagation(t *testing.T) {
	t.Run("Custom", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
			return sip.NewResponseFromRequest(req, 200, "OK", body)
		}, WithMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			SDP:    SDPConfig{SessionName: "AcmePBX"},
		}))

		d, err := dg.Dial(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer d.Close()

		ms := d.dialogMedia.mediaSession
		require.NotNil(t, ms)
		assert.Equal(t, "AcmePBX", ms.SDPSessionName)
		assert.Contains(t, string(ms.LocalSDP()), "s=AcmePBX")
	})

	t.Run("DefaultWhenEmpty", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW}, "")
			return sip.NewResponseFromRequest(req, 200, "OK", body)
		}, WithMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		}))

		d, err := dg.Dial(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer d.Close()

		ms := d.dialogMedia.mediaSession
		require.NotNil(t, ms)
		assert.Empty(t, ms.SDPSessionName, "empty config means library default")
		assert.Contains(t, string(ms.LocalSDP()), "s=Sip Go Media")
	})
}
