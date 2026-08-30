// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/fakes"
	"github.com/stretchr/testify/require"
)

func fakeMediaSessionWriter(lport int, rport int, rtpWriter io.Writer) *MediaSession {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Raddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
	}

	conn := &fakes.UDPConn{
		Writers: map[string]io.Writer{
			sess.Raddr.String(): bytes.NewBuffer([]byte{}),
		},
	}
	sess.rtpConn = conn
	return sess
}

func TestRTPWriter(t *testing.T) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionWriter(0, 1234, rtpConn)
	rtpSession := NewRTPSession(sess)
	rtpWriter := NewRTPPacketWriterSession(rtpSession)

	payload := []byte("12312313")
	N := 10
	for i := 0; i < N; i++ {
		_, err := rtpWriter.Write(payload)
		require.NoError(t, err)

		pkt := rtpWriter.PacketHeader

		require.Equal(t, rtpWriter.codec.PayloadType, pkt.PayloadType)
		require.Equal(t, rtpWriter.SSRC, pkt.SSRC)
		require.Equal(t, rtpWriter.seqWriter.ReadExtendedSeq(), uint64(pkt.SequenceNumber))
		require.Equal(t, rtpWriter.nextTimestamp, pkt.Timestamp+160, "%d vs %d", rtpWriter.nextTimestamp, pkt.Timestamp)
		require.Equal(t, i == 0, pkt.Marker)
	}
}

// Regression: Write used to mutate writer state (nextTimestamp, sequencer,
// PacketHeader) under a read lock, and lastSampleTime was written without any
// lock. Concurrent writers produced torn sequence numbers and raced
func TestRTPWriterConcurrent(t *testing.T) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionWriter(0, 1234, rtpConn)
	rtpSession := NewRTPSession(sess)
	rtpWriter := NewRTPPacketWriterSession(rtpSession)
	rtpWriter.clockTicker.Reset(time.Nanosecond) // Speed up pacing

	const goroutines = 4
	const writes = 50
	seqBefore := rtpWriter.seqWriter.ReadExtendedSeq()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := make([]byte, 160)
			for i := 0; i < writes; i++ {
				if _, err := rtpWriter.Write(payload); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Every write must have consumed exactly one sequence number (allowing wrap)
	seqAfter := rtpWriter.seqWriter.ReadExtendedSeq()
	delta := seqAfter - seqBefore
	require.True(t,
		delta == goroutines*writes || delta == goroutines*writes+65536,
		"sequence delta=%d, want %d", delta, goroutines*writes)
}

func BenchmarkRTPPacketWriter(b *testing.B) {
	reader, writer := io.Pipe()
	session := fakeMediaSessionWriter(0, 1234, writer)
	rtpSess := NewRTPSession(session)
	w := NewRTPPacketWriterSession(rtpSess)
	w.clockTicker.Reset(1 * time.Nanosecond)

	go func() {
		io.ReadAll(reader)
	}()

	data := make([]byte, 160)

	for i := 0; i < b.N; i++ {
		_, err := w.Write(data)
		if err != nil {
			b.Error(err)
		}
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "writes/s")
}
