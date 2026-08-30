// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Pause/interrupt sentinels for the media read/write gates.
//
// The gates replace the legacy pattern of expressing "stop/pause" by expiring
// the shared UDP conn deadlines (StopRTP/StartRTP), where multiple components
// overwrote each other's state. With the gates, the read/write deadline is a
// transient interrupt signal owned by the stable handles; a pause is a
// refcounted state resolved at use time.
var (
	// ErrReadPaused is returned by RTPPacketReader reads while the reader is
	// paused (PauseRead) or interrupted through a context.
	ErrReadPaused = errors.New("rtp read paused")

	// ErrWritePaused is returned by RTPPacketWriter writes while the writer is
	// paused (PauseWrite).
	ErrWritePaused = errors.New("rtp write paused")
)

// readInterrupter is implemented by readers that own a network conn whose
// read can be interrupted by expiring the read deadline. It is resolved by
// RTPPacketReader from its active reader chain and must stay internal.
type readInterrupter interface {
	// pokeReadDeadline expires the read deadline once to unblock an in-flight read.
	pokeReadDeadline()
	// clearReadDeadline restores blocking reads (zero deadline).
	clearReadDeadline()
}

func (s *MediaSession) pokeReadDeadline() {
	if s.rtpConn == nil {
		return
	}
	s.rtpConn.SetReadDeadline(time.Now())
}

func (s *MediaSession) clearReadDeadline() {
	if s.rtpConn == nil {
		return
	}
	s.rtpConn.SetReadDeadline(time.Time{})
}

// readPauser is implemented by RTP readers that can surface ErrReadPaused
// from an in-flight ReadRTP when signaled. Used by readers that do not own
// the conn directly (ex. RTPJitterBuffer, whose pump goroutine must not
// observe conn deadline pokes - a read error there is terminal for the pump).
type readPauser interface {
	setReadPause(paused <-chan struct{})
}

// PauseRead pauses the reader: current and subsequent Read calls return
// ErrReadPaused until the returned release is called.
//
// Pause is refcounted: the reader stays paused until every release is called,
// so concurrent components can not accidentally resume each other (this is
// the fix for the legacy StartRTP "resume stomps everyone" behavior). Release
// must be called exactly once per PauseRead call.
//
// The pause is delivered to the active reader: pause-aware readers (ex.
// RTPJitterBuffer) are signaled directly, session-owned readers are
// interrupted by a one-shot read deadline poke which the last release
// restores. Underlying network buffering is not stopped by a pause.
func (r *RTPPacketReader) PauseRead() (release func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pauseCount.Add(1)
	if r.pauseCount.Load() == 1 {
		ch := make(chan struct{})
		close(ch)
		r.pauseSignal = ch
		r.applyPauseLocked()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.pauseCount.Add(-1) == 0 {
				r.pauseSignal = nil
				r.clearPauseLocked()
			}
		})
	}
}

// paused reports whether the reader is currently paused. Hot path: atomic.
func (r *RTPPacketReader) paused() bool {
	return r.pauseCount.Load() > 0
}

// applyPauseLocked delivers the pause to the active reader. r.mu held.
func (r *RTPPacketReader) applyPauseLocked() {
	if p, ok := r.reader.(readPauser); ok {
		p.setReadPause(r.pauseSignal)
		return
	}
	if r.pokeTarget != nil {
		r.pokeTarget.pokeReadDeadline()
	}
}

// clearPauseLocked restores the reader after the last pause is released. r.mu held.
func (r *RTPPacketReader) clearPauseLocked() {
	if p, ok := r.reader.(readPauser); ok {
		p.setReadPause(nil)
	}
	if r.pokeTarget != nil {
		r.pokeTarget.clearReadDeadline()
	}
}

// interrupt unblocks an in-flight read once. r.mu held internally.
func (r *RTPPacketReader) interrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.reader.(readPauser); ok {
		if r.pauseSignal == nil {
			ch := make(chan struct{})
			close(ch)
			r.pauseSignal = ch
			p.setReadPause(ch)
		}
		// With a pause already active the in-flight read surfaces
		// ErrReadPaused on its own; nothing more to deliver.
		return
	}
	if r.pokeTarget != nil {
		r.pokeTarget.pokeReadDeadline()
	}
}

// restoreInterrupt returns the reader to normal blocking reads after a
// context-driven interrupt. Skipped when a pause took ownership of the state
// (its release restores instead).
func (r *RTPPacketReader) restoreInterrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pauseCount.Load() > 0 {
		return
	}
	if p, ok := r.reader.(readPauser); ok {
		p.setReadPause(nil)
	}
	r.pauseSignal = nil
	if r.pokeTarget != nil {
		r.pokeTarget.clearReadDeadline()
	}
}

// ArmReadInterrupt arranges for an in-flight read to be interrupted when ctx
// is done. The returned disarm joins the watcher and, when the interrupt was
// delivered, restores the reader (unless a pause owns the state). Disarm must
// be called exactly once; it is safe to call after ctx is already done.
func (r *RTPPacketReader) ArmReadInterrupt(ctx context.Context) (disarm func()) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			r.interrupt()
		case <-stop:
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			if ctx.Err() != nil {
				r.restoreInterrupt()
			}
		})
	}
}

// ReadContext reads the next payload, interrupting the underlying blocking
// read when ctx is done. On cancellation it returns ctx.Err(). The reader
// stays fully usable afterwards.
func (r *RTPPacketReader) ReadContext(ctx context.Context, b []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	disarm := r.ArmReadInterrupt(ctx)
	defer disarm()

	n, err := r.Read(b)
	if n > 0 {
		return n, err
	}
	if r.paused() {
		return 0, ErrReadPaused
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	return n, err
}

// PauseWrite pauses the writer: Write and WriteSamples return ErrWritePaused
// until the returned release is called. Refcounted like PauseRead; release
// must be called exactly once per PauseWrite call.
//
// An in-flight write completes first (pacing wait is at most one packet
// interval), so pause latency is bounded by the codec sample duration.
func (p *RTPPacketWriter) PauseWrite() (release func()) {
	p.writePaused.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			p.writePaused.Add(-1)
		})
	}
}

// writePaused reports whether the writer is currently paused. Hot path: atomic.
func (p *RTPPacketWriter) writePausedNow() bool {
	return p.writePaused.Load() > 0
}
