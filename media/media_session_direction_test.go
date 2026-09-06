// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/sjlit/diago/media/sdp"
	"github.com/stretchr/testify/require"
)

// Media direction (RFC 3264) must be enforced symmetrically: sendonly drops
// reads, recvonly drops writes, and inactive drops both.
func TestMediaSessionDirectionGates(t *testing.T) {
	rawPkt := func(t *testing.T) []byte {
		t.Helper()
		pkt := rtp.Packet{Payload: bytes.Repeat([]byte{0xAB}, 160)}
		data, err := pkt.Marshal()
		require.NoError(t, err)
		return data
	}

	t.Run("ReadRTP", func(t *testing.T) {
		raw := rawPkt(t)
		for _, mode := range []string{sdp.ModeSendrecv, sdp.ModeRecvonly} {
			s := &MediaSession{mode: mode}
			s.rtpConn = &fakes.UDPConn{Reader: bytes.NewReader(raw)}

			buf := make([]byte, RTPBufSize)
			pkt := rtp.Packet{}
			n, err := s.ReadRTP(buf, &pkt)
			require.NoError(t, err, "mode %q", mode)
			require.Equal(t, len(raw), n, "mode %q must deliver media", mode)
		}
		for _, mode := range []string{sdp.ModeSendonly, sdp.ModeInactive} {
			s := &MediaSession{mode: mode}
			s.rtpConn = &fakes.UDPConn{Reader: bytes.NewReader(raw)}

			buf := make([]byte, RTPBufSize)
			pkt := rtp.Packet{}
			n, err := s.ReadRTP(buf, &pkt)
			require.NoError(t, err, "mode %q", mode)
			require.Zero(t, n, "mode %q must not deliver media", mode)
		}
	})

	t.Run("WriteRTP", func(t *testing.T) {
		pkt := rtp.Packet{Payload: bytes.Repeat([]byte{0xAB}, 160)}
		for _, mode := range []string{sdp.ModeSendrecv, sdp.ModeSendonly} {
			s, sink := newDirectionWriteSession(mode)
			require.NoError(t, s.WriteRTP(&pkt), "mode %q", mode)
			require.NotZero(t, sink.Len(), "mode %q must send media", mode)
		}
		for _, mode := range []string{sdp.ModeRecvonly, sdp.ModeInactive} {
			s, sink := newDirectionWriteSession(mode)
			require.NoError(t, s.WriteRTP(&pkt), "mode %q", mode)
			require.Zero(t, sink.Len(), "mode %q must not send media", mode)
		}
	})
}

func newDirectionWriteSession(mode string) (*MediaSession, *bytes.Buffer) {
	s := &MediaSession{
		mode:  mode,
		Raddr: net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5004},
	}
	sink := &bytes.Buffer{}
	s.rtpConn = &fakes.UDPConn{
		Writers: map[string]io.Writer{
			s.Raddr.String(): sink,
		},
	}
	return s, sink
}
