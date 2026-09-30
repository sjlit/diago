// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
)

// PlaybackState represents current playback state of AudioPlaybackControl
type PlaybackState uint32

// PlaybackStateIdle, PlaybackStatePlaying, PlaybackStatePaused and
// PlaybackStateStopped are the states reported by AudioPlaybackControl.State.
const (
	PlaybackStateIdle PlaybackState = iota
	PlaybackStatePlaying
	PlaybackStatePaused
	PlaybackStateStopped
)

func (s PlaybackState) String() string {
	switch s {
	case PlaybackStateIdle:
		return "idle"
	case PlaybackStatePlaying:
		return "playing"
	case PlaybackStatePaused:
		return "paused"
	case PlaybackStateStopped:
		return "stopped"
	}
	return "unknown"
}

// stopSignal is returned by audioControl on Stop. It matches io.EOF for
// backward compatibility and ErrPlaybackStopped for detection.
type stopSignal struct{}

func (stopSignal) Error() string { return "audio stopped: " + ErrPlaybackStopped.Error() }

func (stopSignal) Is(target error) bool {
	return target == io.EOF || target == ErrPlaybackStopped
}

// replaySignal is returned by audioControl when replay is requested. It matches
// io.EOF for backward compatibility and ErrPlaybackReplayed for detection.
type replaySignal struct{}

func (replaySignal) Error() string { return "audio replayed: " + ErrPlaybackReplayed.Error() }

func (replaySignal) Is(target error) bool {
	return target == io.EOF || target == ErrPlaybackReplayed
}

// AudioPlaybackControl is playback with mute, stop, pause, resume and
// replay controls.
type AudioPlaybackControl struct {
	AudioPlayback

	control *audioControl
}

// newAudioControl constructs an audioControl with a fresh stop channel. Stop
// closes the channel of the current play generation exactly once; the Pause
// gate listens on it so Stop can interrupt a paused playback without polling.
func newAudioControl(writer io.Writer) *audioControl {
	return &audioControl{
		Writer:   writer,
		stopChan: make(chan struct{}),
	}
}

// NewAudioPlaybackControl wraps a playback with controls. Prefer
// Media.StartPlaybackControl on a dialog.
func NewAudioPlaybackControl(a AudioPlayback) AudioPlaybackControl {
	// Replace audio playback writer with control
	writer := a.writer
	control := newAudioControl(writer)
	a.writer = control
	return AudioPlaybackControl{AudioPlayback: a, control: control}
}

// Mute mutes or unmutes the outgoing audio without stopping playout.
func (p *AudioPlaybackControl) Mute(mute bool) {
	p.control.Mute(mute)
}

// Stop stops playback. Ongoing Play returns error matching ErrPlaybackStopped
// and io.EOF.
func (p *AudioPlaybackControl) Stop() {
	p.control.Stop()
}

// Pause pauses playback. Playback position freezes and no RTP packets are
// sent, which remote side experiences as silence. Use Resume to continue from
// the same position.
func (p *AudioPlaybackControl) Pause() {
	p.control.Pause()
}

// Resume resumes playback paused with Pause
func (p *AudioPlaybackControl) Resume() {
	p.control.Resume()
}

// Replay requests playback restart from the beginning. It MUST be called from
// a different goroutine than Play (ie DTMF callback) and only while playback
// is active. Ongoing Play returns ErrPlaybackReplayed and playback source is
// restarted. Sources opened with PlayFile/PlayURL can always replay. Generic
// Play(reader) requires reader to implement io.Seeker.
func (p *AudioPlaybackControl) Replay() error {
	return p.control.Replay()
}

// State returns current playback state
func (p *AudioPlaybackControl) State() PlaybackState {
	return p.control.State()
}

// Play plays reader content with replay support. On replay request (Replay()
// called from other goroutine) playback restarts if reader implements
// io.Seeker, otherwise error ErrSourceNotReplayable is returned.
// Use PlayFile or PlayURL for non-seekable sources.
//
// Deprecated: Use PlayContext.
func (p *AudioPlaybackControl) Play(reader io.Reader, mimeType string) (int64, error) {
	return p.PlayContext(context.Background(), reader, mimeType)
}

// PlayContext plays reader content with replay support, stopping with ctx.Err()
// when the context is canceled. See Play for the replay semantics.
func (p *AudioPlaybackControl) PlayContext(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		n, err := p.AudioPlayback.PlayContext(ctx, reader, mimeType)
		written += n
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, ctxErr
		}
		if !errors.Is(err, ErrPlaybackReplayed) {
			return written, err
		}
		seeker, ok := reader.(io.Seeker)
		if !ok {
			return written, errors.Join(fmt.Errorf("%w: reader does not support io.Seeker", ErrSourceNotReplayable), err)
		}
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return written, err
		}
	}
}

// PlayFile plays wav file with replay support. On replay request file is
// reopened and playback restarts from the beginning.
//
// Deprecated: Use PlayFileContext.
func (p *AudioPlaybackControl) PlayFile(filename string) (int64, error) {
	return p.PlayFileContext(context.Background(), filename)
}

// PlayFileContext plays wav file with replay support, stopping with ctx.Err()
// when the context is canceled.
func (p *AudioPlaybackControl) PlayFileContext(ctx context.Context, filename string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, ctxErr
		}
		file, err := os.Open(filename)
		if err != nil {
			return written, err
		}
		if ext := path.Ext(filename); ext != ".wav" {
			_ = file.Close()
			return written, fmt.Errorf("only playing wav file is now supported, but detected=%q", ext)
		}

		// Using bufio to improve disk reading
		fileReader := bufio.NewReaderSize(file, 64*1024)
		n, err := p.AudioPlayback.PlayContext(ctx, fileReader, "audio/wav")
		written += n
		_ = file.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, ctxErr
		}
		if !errors.Is(err, ErrPlaybackReplayed) {
			return written, err
		}
	}
}

// PlayURL plays wav content from url with replay support. On replay request
// url is refetched and playback restarts from the beginning.
//
// Deprecated: Use PlayURLContext.
func (p *AudioPlaybackControl) PlayURL(urlStr string) (int64, error) {
	return p.PlayURLContext(context.Background(), urlStr)
}

// PlayURLContext plays wav content from url with replay support, stopping
// with ctx.Err() when the context is canceled.
func (p *AudioPlaybackControl) PlayURLContext(ctx context.Context, urlStr string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		err := p.playURL(ctx, urlStr, &written)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, ctxErr
		}
		switch {
		case errors.Is(err, ErrPlaybackReplayed):
			continue
		case errors.Is(err, ErrPlaybackStopped):
			return written, err
		case errors.Is(err, io.EOF):
			return written, nil
		}
		return written, err
	}
}

/*
	Playback control provides functionality like Mute, Stop, Pause, Resume and Replay over audio.
*/

type audioControl struct {
	Reader io.Reader // MUST be set if used as reader
	Writer io.Writer // Must be set if used as writer

	muted atomic.Bool
	// muteBuf backs muted writes so the caller's buffer stays untouched.
	// Written only from the playback goroutine.
	muteBuf []byte
	// gate blocks writes while paused. nil means not paused, non-nil points
	// to channel closed on resume.
	gate atomic.Pointer[chan struct{}]
	stop atomic.Bool
	// stopChan is the stop channel of the CURRENT play generation; Stop
	// closes it exactly once and enterPlay replaces it, so a playback started
	// after a stop gets a live channel again. stopMu guards the field and
	// makes the "mark stopped + close channel" sequence atomic against
	// enterPlay's "reset + fresh channel".
	stopMu   sync.Mutex
	stopChan chan struct{}
	// replay holds pending replay request consumed by next Write
	replay atomic.Bool
	// active is true while Play is in progress. Guarded by enterPlay/exitPlay.
	active atomic.Bool
	state  atomic.Int32
}

func (c *audioControl) Read(b []byte) (n int, err error) {
	if c.stop.Load() {
		return 0, stopSignal{}
	}

	n, err = c.Reader.Read(b)
	if err != nil {
		return n, err
	}

	if c.muted.Load() {
		for i := range b[:n] {
			b[i] = 0
		}
	}

	return n, err
}

func (c *audioControl) Write(b []byte) (n int, err error) {
	if c.stop.Load() {
		return 0, stopSignal{}
	}

	if c.replay.CompareAndSwap(true, false) {
		return 0, replaySignal{}
	}

	// Pause blocks playback position until resumed. RTP stream pauses with it
	// (no packets sent), which remote side experiences as silence. The stop
	// channel of the current generation is also selected so Stop interrupts a
	// paused playback immediately.
	if gate := c.gate.Load(); gate != nil {
		stopCh := c.stopChanNow()
		for c.gate.Load() != nil {
			select {
			case <-*gate:
			case <-stopCh:
				return 0, stopSignal{}
			}
			if c.stop.Load() {
				return 0, stopSignal{}
			}
		}
	}

	if c.muted.Load() {
		// io.Writer must not modify the caller's buffer; write from a
		// reusable zero-filled buffer instead.
		if cap(c.muteBuf) < len(b) {
			c.muteBuf = make([]byte, len(b))
		}
		b = c.muteBuf[:len(b)]
	}

	return c.Writer.Write(b)
}

func (c *audioControl) Mute(mute bool) {
	c.muted.Store(mute)
}

func (c *audioControl) Pause() {
	ch := make(chan struct{})
	if !c.gate.CompareAndSwap(nil, &ch) {
		return // already paused
	}
	c.state.Store(int32(PlaybackStatePaused))
}

func (c *audioControl) Resume() {
	gate := c.gate.Swap(nil)
	if gate == nil {
		return
	}
	close(*gate)
	if PlaybackState(c.state.Load()) == PlaybackStatePaused {
		c.state.Store(int32(PlaybackStatePlaying))
	}
}

// Stop will stop reader/writer. Write/Read returns error matching
// ErrPlaybackStopped and io.EOF. First call of a play generation closes its
// stop channel so any paused Write returns immediately; subsequent calls are
// no-op. A playback started again after Stop gets its own stop channel.
func (c *audioControl) Stop() {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	if c.stop.Load() {
		return
	}
	c.stop.Store(true)
	close(c.stopChan)
	c.state.Store(int32(PlaybackStateStopped))
}

// stopChanNow returns the stop channel of the current play generation.
func (c *audioControl) stopChanNow() <-chan struct{} {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	return c.stopChan
}

// Replay sets pending replay request. It is consumed by next Write and fails
// if no playback is in progress, or if Stop has already been called.
func (c *audioControl) Replay() error {
	if !c.active.Load() {
		return fmt.Errorf("no playback in progress")
	}
	if c.stop.Load() {
		return fmt.Errorf("playback is stopping")
	}
	c.replay.Store(true)
	return nil
}

// State returns last known playback state
func (c *audioControl) State() PlaybackState {
	return PlaybackState(c.state.Load())
}

func (c *audioControl) enterPlay() {
	// Clear leftovers from previous play and install a live stop channel for
	// this generation (Stop closes the channel of the generation it stopped).
	c.stopMu.Lock()
	c.stopChan = make(chan struct{})
	c.stop.Store(false)
	c.stopMu.Unlock()
	c.Resume()
	c.replay.Store(false)
	c.active.Store(true)
	c.state.Store(int32(PlaybackStatePlaying))
}

func (c *audioControl) exitPlay() {
	c.active.Store(false)
	// Return to idle unless explicitly stopped or paused
	if s := PlaybackState(c.state.Load()); s == PlaybackStatePlaying {
		c.state.Store(int32(PlaybackStateIdle))
	}
}
