// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/pion/rtp"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQueuedRTPDialog returns a server dialog with a real loopback media
// session plus a sender injecting RTP into its socket. The audio pipeline
// heads are in-memory buffers so the proxy loop terminates on EOF instead of
// blocking on the socket, leaving anything queued on the socket observable.
func newQueuedRTPDialog(t *testing.T) (*DialogServerSession, func(n int)) {
	t.Helper()

	msess, err := media.NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { msess.Close() })

	sender, err := net.DialUDP("udp", nil, &msess.Laddr)
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

	d := &DialogServerSession{
		DialogServerSession: &sipgo.DialogServerSession{
			Dialog: sipgo.Dialog{ID: "queued-rtp"},
		},
	}
	d.mediaSession = msess
	d.audioReader = bytes.NewBuffer(nil)
	d.audioWriter = bytes.NewBuffer(nil)
	d.RTPPacketReader = media.NewRTPPacketReader(nil, media.CodecAudioUlaw)
	d.RTPPacketWriter = media.NewRTPPacketWriter(nil, media.CodecAudioUlaw)
	return d, send
}

// Regression: queued pre-bridge RTP must not be replayed by the proxy -
// replaying it at the line rate turns the pre-bridge backlog into permanent
// one-way latency (docs/contracts.md §15). Callers can opt out.
func TestBridgeDiscardQueuedRTPOnStart(t *testing.T) {
	for _, discard := range []bool{true, false} {
		name := "forward_queued"
		if discard {
			name = "discard_queued"
		}
		t.Run(name, func(t *testing.T) {
			b := NewBridge()
			b.WaitDialogsNum = 99 // start the proxy explicitly below
			b.DiscardQueuedOnStart = discard

			incoming, sendIncoming := newQueuedRTPDialog(t)
			outgoing, _ := newQueuedRTPDialog(t)

			require.NoError(t, b.AddDialogSession(incoming))
			require.NoError(t, b.AddDialogSession(outgoing))

			sendIncoming(3)
			// Loopback UDP enqueues on send, but give the receiver a moment
			// so the assertion is deterministic under load.
			time.Sleep(20 * time.Millisecond)

			err := b.proxyMedia()
			require.ErrorIs(t, err, io.EOF) // in-memory readers are empty

			// Whatever remains queued on the socket was not consumed by the
			// proxy (it reads the in-memory heads).
			n, err := incoming.Media().DiscardQueuedRTP()
			require.NoError(t, err)
			if discard {
				assert.Zero(t, n, "bridge must start from live media")
			} else {
				assert.Equal(t, 3, n, "opt-out must keep queued packets")
			}
		})
	}
}
