// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func fakeMediaSessionReader(lport int, rtpReader io.Reader) *MediaSession {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: lport},
	}

	conn := &fakes.UDPConn{
		Reader: rtpReader,
	}
	sess.rtpConn = conn
	return sess
}

func TestRTPReader(t *testing.T) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionReader(0, rtpConn)
	rtpSess := NewRTPSession(sess)
	rtpReader := NewRTPPacketReaderSession(rtpSess)

	payload := []byte("12312313")
	N := 10
	buf := make([]byte, 3200)
	for i := 0; i < N; i++ {
		writePkt := rtp.Packet{
			Header: rtp.Header{
				SSRC:           1234,
				Version:        2,
				PayloadType:    8,
				SequenceNumber: uint16(i),
				Timestamp:      160 * uint32(i),
				Marker:         i == 0,
			},
			Payload: payload,
		}
		data, _ := writePkt.Marshal()
		rtpConn.Reset()
		rtpConn.Write(data)
		// conn.Reader = bytes.NewBuffer(data)

		n, err := rtpReader.Read(buf)
		require.NoError(t, err)

		pkt := rtpReader.PacketHeader
		require.Equal(t, writePkt.PayloadType, pkt.PayloadType)
		require.Equal(t, writePkt.SSRC, pkt.SSRC)
		require.Equal(t, i == 0, pkt.Marker)
		require.Equal(t, len(payload), n)
		require.Equal(t, rtpReader.seqReader.ReadExtendedSeq(), uint64(writePkt.SequenceNumber))
	}
}

func BenchmarkRTPPacketReader(b *testing.B) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionReader(0, rtpConn)
	rtpSess := NewRTPSession(sess)
	rtpReader := NewRTPPacketReaderSession(rtpSess)

	payload := []byte("12312313")
	buf := make([]byte, 3200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writePkt := rtp.Packet{
			Header: rtp.Header{
				SSRC:           1234,
				Version:        2,
				PayloadType:    8,
				SequenceNumber: uint16(i % (1 << 16)),
				Timestamp:      160 * uint32(i),
				Marker:         i == 0,
			},
			Payload: payload,
		}
		data, _ := writePkt.Marshal()
		rtpConn.Write(data)

		_, err := rtpReader.Read(buf)
		require.NoError(b, err)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "reads/s")
}

// Regression: UpdateRTPSession replaced the whole reader with the bare
// RTPSession, silently dropping wrappers installed by the user (ex. the
// jitter buffer) on every media update.
func TestRTPPacketReaderUpdateRTPSessionKeepsJitterBuffer(t *testing.T) {
	oldSess := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	oldRTPSess := NewRTPSession(oldSess)
	newSess := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	newRTPSess := NewRTPSession(newSess)

	reader := NewRTPPacketReaderSession(oldRTPSess)
	jitter := mustNewRTPJitterBuffer(t, oldRTPSess, 20*time.Millisecond, RTPJitterBufferOptions{})
	t.Cleanup(func() { _ = jitter.Close() })
	reader.UpdateReader(jitter)

	reader.UpdateRTPSession(newRTPSess)

	jitterReader, ok := reader.Reader().(*RTPJitterBuffer)
	require.True(t, ok, "jitter buffer must stay in the reader chain")
	require.NotNil(t, jitterReader.reader.Load())
	inner, ok := (*jitterReader.reader.Load()).(*RTPSession)
	require.True(t, ok)
	require.Same(t, newRTPSess, inner)

	// The bare session path still swaps the reader.
	plain := NewRTPPacketReaderSession(oldRTPSess)
	plain.UpdateRTPSession(newRTPSess)
	require.Same(t, newRTPSess, plain.Reader())
}
