// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxSender returns a function delivering one PCMU RTP packet (n bytes
// payload) to the dialog media session.
func ctxSender(t *testing.T, d *DialogMedia) func(n int) {
	t.Helper()
	ms := d.MediaSession()
	require.NotNil(t, ms)
	conn, err := net.DialUDP("udp", nil, &ms.Laddr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	seq := 0
	return func(n int) {
		seq++
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    0, // PCMU
				SequenceNumber: uint16(seq),
				Timestamp:      uint32(seq * 160),
				SSRC:           0xABCD,
			},
			Payload: make([]byte, n),
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		_, _ = conn.Write(data)
	}
}

func readOnePacket(t *testing.T, d *DialogMedia, timeout time.Duration) error {
	t.Helper()
	audioReader, err := d.AudioReader()
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, media.RTPBufSize)
		_, err := audioReader.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return context.DeadlineExceeded
	}
}

// TestDialogMediaPauseRefcount is the core anti-fight property: a pause is
// refcounted, so one component releasing can not resume another's pause.
func TestDialogMediaPauseRefcount(t *testing.T) {
	d := newTestDialogMedia(t)
	send := ctxSender(t, d)

	rel1, err := d.PauseAudioRead()
	require.NoError(t, err)
	rel2, err := d.PauseAudioRead()
	require.NoError(t, err)

	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	require.NoError(t, err)

	// A read while paused surfaces ErrReadPaused immediately.
	_, err = audioReader.Read(buf)
	assert.ErrorIs(t, err, media.ErrReadPaused)

	rel1()
	_, err = audioReader.Read(buf)
	assert.ErrorIs(t, err, media.ErrReadPaused, "one release must not resume the reader while another pause is held")

	rel2()
	// After the last release the reader works again.
	send(160)
	readOk := make(chan error, 1)
	go func() {
		_, err := audioReader.Read(buf)
		readOk <- err
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not resume after all pauses released")
	}
}

// TestDialogMediaListenBackgroundStopContention is a regression test for the
// deadline-fight defect class: stopping ListenBackground while another
// component holds a read pause must not resume that pause, and the reader
// must stay usable after the stop.
func TestDialogMediaListenBackgroundStopContention(t *testing.T) {
	d := newTestDialogMedia(t)
	send := ctxSender(t, d)

	stop, err := d.ListenBackground()
	require.NoError(t, err)

	// External component pauses while ListenBackground is running.
	externalRelease, err := d.PauseAudioRead()
	require.NoError(t, err)

	// Stopping takes its own pause on top of the external one.
	require.NoError(t, stop())
	// The external pause must still be held after the stop returned.
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	require.NoError(t, err)
	_, err = audioReader.Read(buf)
	assert.ErrorIs(t, err, media.ErrReadPaused, "ListenBackground stop must not clear a foreign pause")

	externalRelease()
	send(160)
	readOk := make(chan error, 1)
	go func() {
		_, err := audioReader.Read(buf)
		readOk <- err
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err, "reader must stay usable after ListenBackground stop")
	case <-time.After(2 * time.Second):
		t.Fatal("reader broken after ListenBackground stop")
	}
}

func TestDialogMediaListenContextCancel(t *testing.T) {
	d := newTestDialogMedia(t)
	send := ctxSender(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.ListenContext(ctx)
	}()

	// Let the listen block on a read, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	start := time.Now()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), time.Second, "cancellation must be prompt")
	case <-time.After(3 * time.Second):
		t.Fatal("ListenContext did not return on cancel")
	}

	// Reader usable after cancellation.
	send(160)
	readOk := make(chan error, 1)
	go func() {
		readOk <- readOnePacket(t, d, time.Second)
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reader broken after ListenContext cancellation")
	}
}

func TestDialogMediaEchoContextCancel(t *testing.T) {
	d := newTestDialogMedia(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.EchoContext(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("EchoContext did not return on cancel")
	}
}

// TestDialogMediaCtxAPIsGuards covers the lifecycle sentinels of the ctx
// and pause APIs.
func TestDialogMediaCtxAPIsGuards(t *testing.T) {
	unanswered := &DialogMedia{}
	_, err := unanswered.PauseAudioRead()
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
	_, err = unanswered.PauseAudioWrite()
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
	_, err = unanswered.armReadInterrupt(context.Background())
	assert.ErrorIs(t, err, ErrDialogNotAnswered)
	assert.ErrorIs(t, unanswered.EchoContext(context.Background()), ErrDialogNotAnswered)

	closed := newTestDialogMedia(t)
	require.NoError(t, closed.Close())
	_, err = closed.PauseAudioRead()
	assert.ErrorIs(t, err, ErrDialogClosed)
	_, err = closed.PauseAudioWrite()
	assert.ErrorIs(t, err, ErrDialogClosed)
}

// TestPlaybackDTMF	closeStopsReadLoopWithoutDeadlines: closing the DTMF
// playback interrupts its read loop through the gate and leaves the dialog
// reader usable - no recurring conn deadlines involved anymore.
func TestPlaybackDTMFCloseStopsReadLoopWithoutDeadlines(t *testing.T) {
	d := newTestDialogMedia(t)
	send := ctxSender(t, d)

	pb, err := d.PlaybackDTMFCreate()
	require.NoError(t, err)

	data := make([]byte, 6400)
	playDone := make(chan struct{})
	go func() {
		defer close(playDone)
		_, _ = pb.Play(&slowCtxReader{r: data}, "")
	}()

	// Wait until the read loop is running, then close.
	time.Sleep(100 * time.Millisecond)
	closeStart := time.Now()
	require.NoError(t, pb.Close())
	assert.Less(t, time.Since(closeStart), time.Second, "Close must be prompt")

	// Reader usable after close.
	send(160)
	readOk := make(chan error, 1)
	go func() {
		readOk <- readOnePacket(t, d, time.Second)
	}()
	select {
	case err := <-readOk:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reader broken after PlaybackDTMF close")
	}
}

// slowCtxReader feeds data slowly so Play stays in progress during Close.
type slowCtxReader struct {
	r []byte
	i int
}

func (s *slowCtxReader) Read(b []byte) (int, error) {
	if s.i >= len(s.r) {
		return 0, context.Canceled
	}
	time.Sleep(2 * time.Millisecond)
	n := copy(b, s.r[s.i:s.i+min(160, len(s.r)-s.i)])
	s.i += n
	return n, nil
}
