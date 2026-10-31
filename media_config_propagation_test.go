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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiagoMediaProfileForTransport locks the profile selection contract: a
// transport without a profile uses the withMediaConfig default, and a named
// profile is used as a complete configuration (no field-level inheritance).
func TestDiagoMediaProfileForTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	dg, err := newAgent(withUserAgent(ua),
		withMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			Network: NetworkConfig{
				RTPNAT:       media.RTPNATSymetric,
				RTPPortStart: 16000,
				RTPPortEnd:   16020,
			},
			SRTP: SRTPConfig{Alg: 1},
			SDP:  SDPConfig{CodecPreferLocalOrder: 1},
		}),
		withMediaProfile("sdes", MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			SRTP:   SRTPConfig{Mode: SecureRTPSDES, Alg: 2},
		}),
		withDefaultTransport(),
	)
	require.NoError(t, err)

	// No profile selected: the default config is used as is.
	conf := dg.mediaConfigForTransport(&Transport{Network: "udp"})
	assert.Equal(t, media.RTPNATSymetric, int(conf.Network.RTPNAT), "default RTPNAT must apply")
	assert.Equal(t, SRTPAlg(1), conf.SRTP.Alg)
	assert.Equal(t, 16000, conf.Network.RTPPortStart)
	assert.Equal(t, 16020, conf.Network.RTPPortEnd)
	assert.Equal(t, 1, conf.SDP.CodecPreferLocalOrder)
	assert.Equal(t, SecureRTPNone, conf.SRTP.Mode)

	// Profile selected: it replaces the default wholesale.
	conf = dg.mediaConfigForTransport(&Transport{Network: "udp", Profile: "sdes"})
	assert.Equal(t, SecureRTPSDES, conf.SRTP.Mode)
	assert.Equal(t, SRTPAlg(2), conf.SRTP.Alg)
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}, conf.Codecs)
	assert.Equal(t, RTPNATDisabled, conf.Network.RTPNAT, "profiles do not inherit default fields")
	assert.Zero(t, conf.Network.RTPPortStart)
	assert.Zero(t, conf.SDP.CodecPreferLocalOrder)
}

// TestDiagoMediaProfileTransportAddressFill locks that the transport-derived
// addresses fill profile fields left nil, and never override profile values.
func TestDiagoMediaProfileTransportAddressFill(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	global := MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Network: NetworkConfig{
			BindIP:     net.IPv4(10, 0, 0, 5),
			ExternalIP: net.IPv4(203, 0, 113, 5),
			RTPNAT:     RTPNATLearnSource,
		},
		SRTP: SRTPConfig{Mode: SecureRTPSDES, Alg: 2},
	}
	dg, err := newAgent(withUserAgent(ua),
		withMediaConfig(global),
		withMediaProfile("public", MediaConfig{
			Codecs:  []media.Codec{media.CodecAudioUlaw},
			Network: NetworkConfig{ExternalIP: net.IPv4(203, 0, 113, 9)},
		}),
		withDefaultTransport(),
	)
	require.NoError(t, err)

	// Default profile keeps its explicit addresses.
	conf := dg.mediaConfigForTransport(&Transport{Network: "udp"})
	assert.Equal(t, net.IPv4(10, 0, 0, 5), conf.Network.BindIP)
	assert.Equal(t, net.IPv4(203, 0, 113, 5), conf.Network.ExternalIP)
	assert.Equal(t, RTPNATLearnSource, conf.Network.RTPNAT)

	// A concrete BindHost pins the media bind IP when the profile leaves it
	// nil; the profile ExternalIP wins over the host-derived one.
	conf = dg.mediaConfigForTransport(&Transport{Network: "udp", BindHost: "192.0.2.7", Profile: "public"})
	assert.Equal(t, net.IPv4(192, 0, 2, 7), conf.Network.BindIP)
	assert.Equal(t, net.IPv4(203, 0, 113, 9), conf.Network.ExternalIP)

	// A profile that pins BindIP is never overridden by the transport.
	conf = dg.mediaConfigForTransport(&Transport{Network: "udp", BindHost: "192.0.2.7"})
	assert.Equal(t, net.IPv4(10, 0, 0, 5), conf.Network.BindIP, "profile BindIP wins over BindHost")
}

// TestDiagoMediaProfileUnknownTransport locks that New rejects a transport
// naming an unregistered profile.
func TestDiagoMediaProfileUnknownTransport(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	_, err = newAgent(withUserAgent(ua), withTransport(Transport{
		Network: "udp", BindHost: "127.0.0.1", BindPort: 0,
		Profile: "missing",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `media profile "missing" is not registered`)
}

// TestDiagoMediaConfigAppliesToSession locks that global MediaConfig settings
// reach the created media session end to end.
func TestDiagoMediaConfigAppliesToSession(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
	}, withMediaConfig(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		Network: NetworkConfig{
			RTPNAT:       media.RTPNATSymetric,
			RTPPortStart: 18000,
			RTPPortEnd:   18020,
		},
	}))

	d, err := dg.DialURI(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
	require.NoError(t, err)
	defer d.release()

	ms := d.callMedia.mediaSession
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
	t.Run("custom", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
		}, withMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			SDP:    SDPConfig{SessionName: "AcmePBX"},
		}))

		d, err := dg.DialURI(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer d.release()

		ms := d.callMedia.mediaSession
		require.NotNil(t, ms)
		assert.Equal(t, "AcmePBX", ms.SDPSessionName)
		assert.Contains(t, string(ms.LocalSDP()), "s=AcmePBX")
	})

	t.Run("default when empty", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", shutdownTestSDP())
		}, withMediaConfig(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		}))

		d, err := dg.DialURI(context.TODO(), sip.Uri{User: "alice", Host: "localhost"})
		require.NoError(t, err)
		defer d.release()

		ms := d.callMedia.mediaSession
		require.NotNil(t, ms)
		assert.Empty(t, ms.SDPSessionName, "empty config means library default")
		assert.Contains(t, string(ms.LocalSDP()), "s=Sip Go Media")
	})
}
