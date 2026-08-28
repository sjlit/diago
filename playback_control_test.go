// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjlit/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countWriter counts writes and total bytes, with optional delay and inner writer
type countWriter struct {
	inner io.Writer
	delay time.Duration

	mu     sync.Mutex
	writes int64
	bytes  int64
}

func (w *countWriter) Write(b []byte) (int, error) {
	time.Sleep(w.delay)
	if w.inner != nil {
		if _, err := w.inner.Write(b); err != nil {
			return 0, err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	w.bytes += int64(len(b))
	return len(b), nil
}

func (w *countWriter) stats() (writes int64, bytes int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.bytes
}

// slowReader delays each read, so playback is paced without RTP clock
type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (r *slowReader) Read(b []byte) (int, error) {
	time.Sleep(r.delay)
	return r.r.Read(b)
}

func (r *slowReader) Seek(offset int64, whence int) (int64, error) {
	if s, ok := r.r.(io.Seeker); ok {
		return s.Seek(offset, whence)
	}
	return 0, errors.New("not seekable")
}

// noSeekReader hides Seek method
type noSeekReader struct {
	r io.Reader
}

func (r *noSeekReader) Read(b []byte) (int, error) {
	return r.r.Read(b)
}

// trackReader tracks consumed bytes
type trackReader struct {
	r       io.Reader
	consumN atomic.Int64
}

func (r *trackReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.consumN.Add(int64(n))
	return n, err
}

func (r *trackReader) consumed() int64 {
	return r.consumN.Load()
}

func newTestControl(w io.Writer) AudioPlaybackControl {
	codec := media.CodecAudioUlaw
	return NewAudioPlaybackControl(NewAudioPlayback(w, codec))
}

func waitPlaying(t *testing.T, p *AudioPlaybackControl) {
	t.Helper()
	require.Eventually(t, func() bool {
		return p.State() == PlaybackStatePlaying
	}, 2*time.Second, time.Millisecond)
}

func TestPlaybackControlStopDuringPlay(t *testing.T) {
	data := make([]byte, 6400) // 20 x 320B reads
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	var written int64
	go func() {
		var err error
		written, err = p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
		// Also matches io.EOF for backward compatibility
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(3 * time.Second):
		t.Fatal("play did not finish")
	}

	_, bytesW := cw.stats()
	assert.Less(t, bytesW, int64(len(data)), "play should be interrupted before end")
	assert.Greater(t, written, int64(0))
	assert.Equal(t, PlaybackStateStopped, p.State())

	// New play is not affected by previous stop
	written2, err := p.Play(bytes.NewReader(data), "")
	require.NoError(t, err)
	assert.EqualValues(t, len(data), written2)
	assert.Equal(t, PlaybackStateIdle, p.State())
}

func TestPlaybackControlStopDuringPause(t *testing.T) {
	data := make([]byte, 6400)
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	p.Pause()
	assert.Equal(t, PlaybackStatePaused, p.State())

	// Stop must unblock paused write
	p.Stop()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(3 * time.Second):
		t.Fatal("play did not finish after stop")
	}
	// Resume is no-op after stop
	p.Resume()
	assert.Equal(t, PlaybackStateStopped, p.State())
}

func TestPlaybackControlPauseResume(t *testing.T) {
	data := make([]byte, 6400)
	src := &trackReader{r: bytes.NewReader(data)}
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: src, delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	require.Eventually(t, func() bool {
		return src.consumed() > 960
	}, 2*time.Second, time.Millisecond)

	p.Pause()
	assert.Equal(t, PlaybackStatePaused, p.State())
	consumedAtPause := src.consumed()

	// While paused source does not advance, one chunk can be in flight
	time.Sleep(60 * time.Millisecond)
	assert.LessOrEqual(t, src.consumed(), consumedAtPause+640)

	p.Resume()
	assert.Equal(t, PlaybackStatePlaying, p.State())

	require.Eventually(t, func() bool {
		return src.consumed() >= int64(len(data))
	}, 3*time.Second, time.Millisecond)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("play did not finish")
	}
	assert.Equal(t, PlaybackStateIdle, p.State())
}

func TestPlaybackControlReplaySeeker(t *testing.T) {
	data := make([]byte, 3200)
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	var written int64
	go func() {
		var err error
		written, err = p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 320
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, p.Replay())

	select {
	case err := <-done:
		// Natural finish after one replay
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("play did not finish")
	}
	// Replayed round is partial + full source, so always more than one pass
	assert.Greater(t, written, int64(len(data)))
	assert.Equal(t, PlaybackStateIdle, p.State())

	// Replay without active play fails
	require.Error(t, p.Replay())
}

func TestPlaybackControlReplayNotReplayable(t *testing.T) {
	data := make([]byte, 3200)
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&noSeekReader{r: &slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	require.Eventually(t, func() bool {
		_, bytesW := cw.stats()
		return bytesW > 320
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, p.Replay())

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrSourceNotReplayable)
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(5 * time.Second):
		t.Fatal("play did not finish")
	}
}

func TestPlaybackControlReplayFile(t *testing.T) {
	dialog := func() *DialogServerSession {
		return &DialogServerSession{
			DialogMedia: DialogMedia{
				mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
				audioWriter:     &countWriter{delay: time.Millisecond},
				RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
			},
		}
	}

	// Measure single pass first
	single, err := dialog().PlaybackControlCreate()
	require.NoError(t, err)
	singleWritten, err := single.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	require.Greater(t, singleWritten, int64(10000))

	// Replay once, so at least double amount should be written
	playback, err := dialog().PlaybackControlCreate()
	require.NoError(t, err)

	done := make(chan error, 1)
	var written int64
	go func() {
		var err error
		written, err = playback.PlayFile("testdata/files/demo-echodone.wav")
		done <- err
	}()

	waitPlaying(t, &playback)
	require.Eventually(t, func() bool {
		_, bytesW := playback.control.Writer.(*countWriter).stats()
		return bytesW > 0
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, playback.Replay())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("play did not finish")
	}
	// Replayed round is partial + full source, so always more than one pass
	assert.Greater(t, written, singleWritten)
}

func TestStopSignalEOFCompatibility(t *testing.T) {
	var err error = stopSignal{}
	require.True(t, errors.Is(err, ErrPlaybackStopped))
	require.True(t, errors.Is(err, io.EOF))

	err = replaySignal{}
	require.True(t, errors.Is(err, ErrPlaybackReplayed))
	require.True(t, errors.Is(err, io.EOF))
}

// TestPlaybackControlStopWhilePausedExitsImmediately verifies that Stop()
// unblocks a Write that is currently parked inside the pause gate within
// microseconds, not after the previous 10ms polling delay.
func TestPlaybackControlStopWhilePausedExitsImmediately(t *testing.T) {
	data := make([]byte, 32000)
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	require.Eventually(t, func() bool {
		_, b := cw.stats()
		return b > 320
	}, 2*time.Second, time.Millisecond)

	p.Pause()
	require.Equal(t, PlaybackStatePaused, p.State())

	// Sample across the gate select phase so the worst case is observed.
	time.Sleep(30 * time.Millisecond)

	t0 := time.Now()
	p.Stop()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(time.Second):
		t.Fatal("play did not finish after stop")
	}
	elapsed := time.Since(t0)
	require.Less(t, elapsed, 5*time.Millisecond,
		"Stop during pause should be near-instant; got %v", elapsed)
}

// TestPlaybackControlReplayAfterStopRejected ensures Replay() returns an
// error when called after Stop() so the caller can detect the silent loss.
func TestPlaybackControlReplayAfterStopRejected(t *testing.T) {
	data := make([]byte, 6400)
	cw := &countWriter{}
	p := newTestControl(cw)

	done := make(chan error, 1)
	go func() {
		_, err := p.Play(&slowReader{r: bytes.NewReader(data), delay: 2 * time.Millisecond}, "")
		done <- err
	}()

	waitPlaying(t, &p)
	require.Eventually(t, func() bool {
		_, b := cw.stats()
		return b > 320
	}, 2*time.Second, time.Millisecond)

	p.Stop()
	// Replay must observe stop state and refuse, even though active is still
	// true until exitPlay runs.
	require.Eventually(t, func() bool {
		return p.control.stop.Load()
	}, time.Second, time.Millisecond)
	require.Error(t, p.Replay(), "Replay must fail after Stop")

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrPlaybackStopped)
	case <-time.After(2 * time.Second):
		t.Fatal("play did not finish")
	}

	// And once exitPlay runs, Replay must continue to fail with the original
	// "no playback in progress" error.
	require.Eventually(t, func() bool {
		err := p.Replay()
		return err != nil && err.Error() != ""
	}, time.Second, time.Millisecond)
}
