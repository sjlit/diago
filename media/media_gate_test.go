// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGateTestReader returns an RTPPacketReader over a loopback media session
// plus a packet sender function. Cleanup closes the session.
func newGateTestReader(t *testing.T) (r *RTPPacketReader, send func(n int), laddr net.UDPAddr) {
	t.Helper()

	sess, err := NewMediaSession(net.ParseIP("127.0.0.1"), 0)
	require.NoError(t, err)
	t.Cleanup(func() { sess.Close() })

	rtpSess := NewRTPSession(sess)
	reader := NewRTPPacketReaderSession(rtpSess)

	conn, err := net.DialUDP("udp", nil, &sess.Laddr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	seq := atomic.Uint32{}
	ts := atomic.Uint32{}
	send = func(n int) {
		s := seq.Add(1)
		t := ts.Add(uint32(n))
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    0, // PCMU, part of the default codec list
				SequenceNumber: uint16(s),
				Timestamp:      t,
				SSRC:           0x1234,
			},
			Payload: make([]byte, n),
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		_, _ = conn.Write(data)
	}
	return reader, send, sess.Laddr
}

func TestRTPPacketReaderPauseBlockedRead(t *testing.T) {
	reader, send, _ := newGateTestReader(t)

	// Block a read with no traffic, then pause from another goroutine.
	blocked := make(chan error, 1)
	go func() {
		buf := make([]byte, RTPBufSize)
		_, err := reader.Read(buf)
		blocked <- err
	}()

	// Give the reader goroutine time to block on the conn.
	time.Sleep(50 * time.Millisecond)

	release := reader.PauseRead()
	select {
	case err := <-blocked:
		assert.ErrorIs(t, err, ErrReadPaused)
	case <-time.After(2 * time.Second):
		t.Fatal("pause did not interrupt the blocked read")
	}

	// While paused, new reads return immediately.
	buf := make([]byte, RTPBufSize)
	_, err := reader.Read(buf)
	assert.ErrorIs(t, err, ErrReadPaused)

	// Release restores normal reads.
	release()
	send(160)
	readOk := make(chan error, 1)
	go func() {
		_, err := reader.Read(buf)
		readOk <- err
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not resume after release")
	}
}

func TestRTPPacketReaderPauseRefcount(t *testing.T) {
	reader, _, _ := newGateTestReader(t)

	rel1 := reader.PauseRead()
	rel2 := reader.PauseRead()

	buf := make([]byte, RTPBufSize)
	_, err := reader.Read(buf)
	assert.ErrorIs(t, err, ErrReadPaused)

	rel1()
	_, err = reader.Read(buf)
	assert.ErrorIs(t, err, ErrReadPaused, "reader must stay paused until the last release")

	rel2()
	assert.False(t, reader.paused())

	// Double release must not corrupt the refcount (once semantics).
	rel2()
	assert.False(t, reader.paused())
}

func TestRTPPacketReaderPauseConcurrent(t *testing.T) {
	reader, send, _ := newGateTestReader(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan struct{})
	var received atomic.Int64
	var wg sync.WaitGroup

	// Reader loop: pauses, cancellations and stray timeout pokes are all
	// retried; the loop exits via stop + ctx cancel unblocking the read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, RTPBufSize)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := reader.ReadContext(ctx, buf)
			if err == nil && n > 0 {
				received.Add(1)
			}
		}
	}()

	// Concurrent pausers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rel := reader.PauseRead()
				send(160)
				time.Sleep(time.Millisecond)
				rel()
			}
		}()
	}

	// Keep traffic flowing so the reader loop keeps completing reads.
	ticker := time.NewTicker(5 * time.Millisecond)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				send(160)
			case <-stop:
				return
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	cancel()
	wg.Wait()
	assert.False(t, reader.paused(), "all pauses must be released")
	assert.Greater(t, received.Load(), int64(0), "reader must have delivered packets between pauses")
}

func TestRTPPacketReaderReadContext(t *testing.T) {
	reader, send, _ := newGateTestReader(t)

	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan error, 1)
	go func() {
		buf := make([]byte, RTPBufSize)
		_, err := reader.ReadContext(ctx, buf)
		blocked <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-blocked:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not interrupt the blocked read")
	}

	// Reader stays usable after cancellation.
	send(160)
	buf := make([]byte, RTPBufSize)
	readOk := make(chan error, 1)
	go func() {
		_, err := reader.Read(buf)
		readOk <- err
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reader broken after ReadContext cancellation")
	}
}

// TestRTPPacketReaderPauseWithJitter is the critical interplay regression:
// the jitter pump must survive pauses (a conn deadline poke would kill it,
// since any read error is terminal for the pump goroutine).
func TestRTPPacketReaderPauseWithJitter(t *testing.T) {
	reader, send, _ := newGateTestReader(t)

	// Install a jitter buffer over the RTP session, like
	// WithAudioReaderJitterBuffer does.
	jitter := NewRTPJitterBuffer(reader.Reader(), 20*time.Millisecond, RTPJitterBufferOptions{
		DelayPackets: 2,
		MaxPackets:   4,
	})
	t.Cleanup(func() { jitter.Close() })
	reader.UpdateReader(jitter)

	buf := make([]byte, RTPBufSize)

	// Prime the jitter with a couple of packets and read one.
	send(160)
	send(160)
	_, err := reader.Read(buf)
	require.NoError(t, err)

	// Pause during active jitter delivery.
	release := reader.PauseRead()
	_, err = reader.Read(buf)
	assert.ErrorIs(t, err, ErrReadPaused)

	// Pump must still be alive: release and read again.
	release()
	send(160)
	send(160)
	readOk := make(chan error, 1)
	go func() {
		_, err := reader.Read(buf)
		readOk <- err
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err, "jitter pump died across pause/release")
	case <-time.After(2 * time.Second):
		t.Fatal("jitter pump died across pause/release")
	}
}

func TestRTPPacketWriterPause(t *testing.T) {
	reader, _, _ := newGateTestReader(t)

	sess := reader.pokeTarget.(*MediaSession)
	writer := NewRTPPacketWriterSession(NewRTPSession(sess))

	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, Timestamp: 160, SSRC: 0x9999},
		Payload: make([]byte, 160),
	}

	release := writer.PauseWrite()
	_, err := writer.Write(pkt.Payload)
	assert.ErrorIs(t, err, ErrWritePaused)

	_, err = writer.WriteSamples(pkt.Payload, 160, false, 0)
	assert.ErrorIs(t, err, ErrWritePaused)

	release()
	assert.False(t, writer.writePausedNow())
}
