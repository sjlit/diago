// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDiscardTestSession creates a loopback media session plus an RTP sender
// aimed at its socket.
func newDiscardTestSession(t *testing.T) (*MediaSession, func(n int)) {
	t.Helper()

	sess, err := NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { sess.Close() })

	sender, err := net.DialUDP("udp", nil, &sess.Laddr)
	require.NoError(t, err)
	t.Cleanup(func() { sender.Close() })

	seq := uint16(0)
	send := func(n int) {
		for range n {
			seq++
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: seq,
					Timestamp:      uint32(seq) * 160,
					SSRC:           0x4242,
				},
				Payload: make([]byte, 160),
			}
			data, err := pkt.Marshal()
			require.NoError(t, err)
			_, err = sender.Write(data)
			require.NoError(t, err)
		}
	}
	return sess, send
}

func TestMediaSessionDiscardQueuedRTP(t *testing.T) {
	sess, send := newDiscardTestSession(t)

	// Empty socket: returns promptly with a zero count.
	n, err := sess.DiscardQueuedRTP()
	require.NoError(t, err)
	assert.Zero(t, n)

	send(3)
	// Loopback UDP enqueues on send, but give the receiver a moment so the
	// assertion is deterministic under load.
	time.Sleep(20 * time.Millisecond)

	n, err = sess.DiscardQueuedRTP()
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// A second drain finds the socket empty.
	n, err = sess.DiscardQueuedRTP()
	require.NoError(t, err)
	assert.Zero(t, n)

	// The read deadline must be restored: the next live packet is readable.
	send(1)
	buf := make([]byte, RTPBufSize)
	pkt := rtp.Packet{}
	_, err = sess.ReadRTP(buf, &pkt)
	require.NoError(t, err)
	assert.Equal(t, uint16(4), pkt.SequenceNumber)
}

func TestMediaSessionDiscardQueuedRTPClosedConn(t *testing.T) {
	sess, _ := newDiscardTestSession(t)
	require.NoError(t, sess.Close())

	_, err := sess.DiscardQueuedRTP()
	assert.Error(t, err)
}

func TestMediaSessionDiscardQueuedRTPDTLSPendingHandshake(t *testing.T) {
	sess, send := newDiscardTestSession(t)
	// DTLS negotiated (SecureRTP == 2) but Finalize has not run yet, so queued
	// datagrams may be DTLS flights rather than SRTP.
	sess.SecureRTP = 2

	send(3)
	// Loopback UDP enqueues on send, but give the receiver a moment so the
	// assertion is deterministic under load.
	time.Sleep(20 * time.Millisecond)

	n, err := sess.DiscardQueuedRTP()
	require.NoError(t, err)
	assert.Zero(t, n, "must not drain while DTLS handshake records may be queued")

	// The datagrams stay queued for the handshake/reader.
	buf := make([]byte, RTPBufSize)
	pkt := rtp.Packet{}
	_, err = sess.ReadRTP(buf, &pkt)
	require.NoError(t, err)
	assert.Equal(t, uint16(1), pkt.SequenceNumber)

	// Once the handshake is done the same session drains normally.
	sess.dtlsEstablished = true
	n, err = sess.DiscardQueuedRTP()
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}
