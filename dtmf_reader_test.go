// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"io"
	"sync/atomic"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/sjlit/diago/media"
)

// packetReadStub returns one payload per Read call, like the per-RTP-packet
// reads the audio chain delivers.
type packetReadStub struct {
	payloads [][]byte
	i        int
}

func (r *packetReadStub) Read(p []byte) (int, error) {
	if r.i >= len(r.payloads) {
		return 0, io.EOF
	}
	n := copy(p, r.payloads[r.i])
	r.i++
	return n, nil
}

// newInbandDTMFReader builds a DTMFReader over a scripted pair of RFC 4733
// packets (start with marker, end without): one DTMF digit '5'.
func newInbandDTMFReader(onDTMF func(dtmf rune) error) (*DTMFReader, *media.RTPPacketReader) {
	packetReader := &media.RTPPacketReader{}
	reader := &DTMFReader{
		dtmfReader: media.NewRTPDTMFReader(media.CodecTelephoneEvent8000, packetReader, &packetReadStub{
			payloads: [][]byte{
				media.DTMFEncode(media.DTMFEvent{Event: 5, Volume: 10, Duration: 160}),
				media.DTMFEncode(media.DTMFEvent{Event: 5, Volume: 10, Duration: 320, EndOfEvent: true}),
			},
		}),
		onDTMF: onDTMF,
	}
	return reader, packetReader
}

// TestDTMFReaderReadWithoutCallback: the in-band DTMF path must not panic
// when no OnDTMF callback is wired - the digit is dropped instead.
func TestDTMFReaderReadWithoutCallback(t *testing.T) {
	reader, packetReader := newInbandDTMFReader(nil)
	buf := make([]byte, media.RTPBufSize)

	// Start of the event: marker packet, no digit complete yet
	packetReader.PacketHeader = rtp.Header{Marker: true, PayloadType: media.CodecTelephoneEvent8000.PayloadType, Timestamp: 1000}
	n, err := reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)

	// End of event: digit completes here and hits the nil callback guard
	packetReader.PacketHeader = rtp.Header{PayloadType: media.CodecTelephoneEvent8000.PayloadType, Timestamp: 1160}
	n, err = reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)
}

// Control for the fixture above: the same packet sequence with a callback
// wired must deliver the digit, proving the nil-callback test exercises the
// callback path and not an undetected event.
func TestDTMFReaderReadWithCallback(t *testing.T) {
	var got atomic.Int32
	reader, packetReader := newInbandDTMFReader(func(dtmf rune) error {
		require.Equal(t, '5', dtmf)
		got.Add(1)
		return nil
	})
	buf := make([]byte, media.RTPBufSize)

	packetReader.PacketHeader = rtp.Header{Marker: true, PayloadType: media.CodecTelephoneEvent8000.PayloadType, Timestamp: 1000}
	_, err := reader.Read(buf)
	require.NoError(t, err)

	packetReader.PacketHeader = rtp.Header{PayloadType: media.CodecTelephoneEvent8000.PayloadType, Timestamp: 1160}
	_, err = reader.Read(buf)
	require.NoError(t, err)

	require.EqualValues(t, 1, got.Load(), "expected exactly one DTMF digit delivered")
}
