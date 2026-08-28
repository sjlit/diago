// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync/atomic"
)

// PlaybackState represents current playback state of AudioPlaybackControl
type PlaybackState uint32

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

type AudioPlaybackControl struct {
	AudioPlayback

	control *audioControl
}

// newAudioControl constructs an audioControl with stopCh initialized. Stop
// closes stopCh exactly once; the Pause gate listens on it so Stop can
// interrupt a paused playback without polling latency.
func newAudioControl(writer io.Writer) *audioControl {
	return &audioControl{
		Writer: writer,
		stopCh: make(chan struct{}),
	}
}

func NewAudioPlaybackControl(a AudioPlayback) AudioPlaybackControl {
	// Replace audio playback writer with control
	writer := a.writer
	control := newAudioControl(writer)
	a.writer = control
	return AudioPlaybackControl{AudioPlayback: a, control: control}
}

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
func (p *AudioPlaybackControl) Play(reader io.Reader, mimeType string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		n, err := p.AudioPlayback.Play(reader, mimeType)
		written += n
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
func (p *AudioPlaybackControl) PlayFile(filename string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		file, err := os.Open(filename)
		if err != nil {
			return written, err
		}
		if ext := path.Ext(filename); ext != ".wav" {
			file.Close()
			return written, fmt.Errorf("only playing wav file is now supported, but detected=%s", ext)
		}

		// Using bufio to improve disk reading
		fileReader := bufio.NewReaderSize(file, 64*1024)
		n, err := p.AudioPlayback.Play(fileReader, "audio/wav")
		written += n
		file.Close()
		if !errors.Is(err, ErrPlaybackReplayed) {
			return written, err
		}
	}
}

// PlayURL plays wav content from url with replay support. On replay request
// url is refetched and playback restarts from the beginning.
func (p *AudioPlaybackControl) PlayURL(urlStr string) (int64, error) {
	p.control.enterPlay()
	defer p.control.exitPlay()

	var written int64
	for {
		err := p.playURL(urlStr, &written)
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
	// gate blocks writes while paused. nil means not paused, non-nil points
	// to channel closed on resume.
	gate atomic.Pointer[chan struct{}]
	stop atomic.Bool
	// stopCh is closed exactly once by Stop. Pause gate listens on it so
	// Stop interrupts a paused playback without polling latency.
	stopCh chan struct{}
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
	// (no packets sent), which remote side experiences as silence. stopCh is
	// also selected so Stop interrupts a paused playback immediately.
	if gate := c.gate.Load(); gate != nil {
		for c.gate.Load() != nil {
			select {
			case <-*gate:
			case <-c.stopCh:
				return 0, stopSignal{}
			}
			if c.stop.Load() {
				return 0, stopSignal{}
			}
		}
	}

	if c.muted.Load() {
		for i := range b {
			b[i] = 0
		}
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
// ErrPlaybackStopped and io.EOF. First call closes stopCh so any paused
// Write returns immediately. Subsequent calls are no-op.
func (c *audioControl) Stop() {
	if c.stop.CompareAndSwap(false, true) {
		close(c.stopCh)
		c.state.Store(int32(PlaybackStateStopped))
	}
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
	// Clear leftovers from previous play
	c.Resume()
	c.stop.Store(false)
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
