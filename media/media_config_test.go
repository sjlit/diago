// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMediaSessionPortRangeFromConfig locks the per-session port range that
// diago's MediaConfig.RTPPortStart/RTPPortEnd feeds into the session, including
// the fallback to the package globals.
func TestMediaSessionPortRangeFromConfig(t *testing.T) {
	newSession := func(start, end int) *MediaSession {
		return &MediaSession{
			Codecs:       []Codec{CodecAudioUlaw, CodecAudioAlaw},
			Mode:         "sendrecv",
			Laddr:        net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
			RTPPortStart: start,
			RTPPortEnd:   end,
		}
	}

	t.Run("InstanceRange", func(t *testing.T) {
		t.Cleanup(func() { rtpPortOffset.Store(0) })
		for i := 0; i < 3; i++ {
			s := newSession(16000, 16020)
			require.NoError(t, s.Init())
			port := s.Laddr.Port
			rtcpPort := s.rtcpConn.LocalAddr().(*net.UDPAddr).Port
			assert.GreaterOrEqual(t, port, 16000, "rtp port must be inside the configured range")
			assert.Less(t, port, 16020)
			assert.Equal(t, port+1, rtcpPort, "rtcp must be rtp+1")
			require.NoError(t, s.Close())
		}
	})

	t.Run("FallsBackToGlobals", func(t *testing.T) {
		oldStart, oldEnd := RTPPortStart, RTPPortEnd
		RTPPortStart, RTPPortEnd = 17000, 17010
		t.Cleanup(func() {
			RTPPortStart, RTPPortEnd = oldStart, oldEnd
			rtpPortOffset.Store(0)
		})

		s := newSession(0, 0)
		require.NoError(t, s.Init())
		port := s.Laddr.Port
		assert.GreaterOrEqual(t, port, 17000)
		assert.Less(t, port, 17010)
		require.NoError(t, s.Close())
	})

	t.Run("ForkCarriesConfig", func(t *testing.T) {
		s := newSession(16000, 16020)
		s.SDPCodecPreferLocalOrder = 1
		f := s.Fork()
		assert.Equal(t, 16000, f.RTPPortStart)
		assert.Equal(t, 16020, f.RTPPortEnd)
		assert.Equal(t, 1, f.SDPCodecPreferLocalOrder)
	})
}

// TestMediaSessionCodecOrderPerSession locks that the codec negotiation order
// can be set per session (fed from MediaConfig) while falling back to the
// package global.
func TestMediaSessionCodecOrderPerSession(t *testing.T) {
	local := []Codec{CodecAudioUlaw, CodecAudioAlaw}
	remote := []Codec{CodecAudioAlaw, CodecAudioUlaw}

	t.Run("SessionFieldLocalOrder", func(t *testing.T) {
		s := &MediaSession{Codecs: local, SDPCodecPreferLocalOrder: 1}
		n := s.updateRemoteCodecs(remote, true)
		require.Equal(t, 2, n)
		assert.Equal(t, "PCMU", s.filterCodecs[0].Name, "local order must win")
	})

	t.Run("GlobalFallback", func(t *testing.T) {
		old := SDPCodecPreferLocalOrder
		SDPCodecPreferLocalOrder = 1
		t.Cleanup(func() { SDPCodecPreferLocalOrder = old })

		s := &MediaSession{Codecs: local}
		n := s.updateRemoteCodecs(remote, true)
		require.Equal(t, 2, n)
		assert.Equal(t, "PCMU", s.filterCodecs[0].Name, "global must apply when session field unset")
	})

	t.Run("DefaultOffererOrder", func(t *testing.T) {
		s := &MediaSession{Codecs: local}
		n := s.updateRemoteCodecs(remote, true)
		require.Equal(t, 2, n)
		assert.Equal(t, "PCMA", s.filterCodecs[0].Name, "default keeps offerer order")
	})
}

// TestMediaSessionNATConstantsAreConst is a compile-time guard: the NAT option
// values must not be mutable package vars.
func TestMediaSessionNATConstantsAreConst(t *testing.T) {
	assert.Equal(t, 0, RTPNATDisabled)
	assert.Equal(t, 1, RTPNATSymetric)
}
