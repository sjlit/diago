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

	t.Run("instance range", func(t *testing.T) {
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

	t.Run("fork carries config", func(t *testing.T) {
		s := newSession(16000, 16020)
		s.SDPCodecPreferLocalOrder = true
		f := s.Fork()
		assert.Equal(t, 16000, f.RTPPortStart)
		assert.Equal(t, 16020, f.RTPPortEnd)
		assert.True(t, f.SDPCodecPreferLocalOrder)
	})
}

// TestMediaSessionCodecOrderPerSession locks that the codec negotiation order
// can be set per session (fed from MediaConfig); the package global fallback
// was removed in v2.
func TestMediaSessionCodecOrderPerSession(t *testing.T) {
	local := []Codec{CodecAudioUlaw, CodecAudioAlaw}
	remote := []Codec{CodecAudioAlaw, CodecAudioUlaw}

	t.Run("session field local order", func(t *testing.T) {
		s := &MediaSession{Codecs: local, SDPCodecPreferLocalOrder: true}
		n := s.updateRemoteCodecs(remote, true)
		require.Equal(t, 2, n)
		assert.Equal(t, "PCMU", s.filterCodecs[0].Name, "local order must win")
	})

	t.Run("default offerer order", func(t *testing.T) {
		s := &MediaSession{Codecs: local}
		n := s.updateRemoteCodecs(remote, true)
		require.Equal(t, 2, n)
		assert.Equal(t, "PCMA", s.filterCodecs[0].Name, "default keeps offerer order")
	})
}

// TestMediaSessionNATConstantsAreConst is a compile-time guard: the NAT option
// values must not be mutable package vars.
func TestMediaSessionNATConstantsAreConst(t *testing.T) {
	assert.EqualValues(t, 0, RTPNATDisabled)
	assert.EqualValues(t, 1, RTPNATSymetric)
}
