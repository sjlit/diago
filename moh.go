// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
	"github.com/sjlit/diago/media/sdp"
)

// MusicOnHold is a handle to a running hold-music loop on one dialog. Create
// it with DialogMedia.PlayMusicOnHold and stop it with Stop (or
// DialogMedia.StopMusicOnHold). The handle is safe for concurrent use.
//
// The loop re-resolves the dialog writer and negotiated codec every frame
// (docs/contracts.md §4), so it survives re-INVITEs and re-renders the tone at
// a changed sample rate without a resampler. It assumes the exclusive write
// path while active (docs/contracts.md §5 single-writer rule): stop other
// playback before starting hold music.
type MusicOnHold struct {
	// auto marks handles started by Hold; Unhold stops only those.
	auto   bool
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

// Done is closed when the loop exits — on Stop, on ctx cancellation, or when
// the loop fails on its own (query the error via Stop).
func (m *MusicOnHold) Done() <-chan struct{} {
	return m.done
}

// Stop cancels the loop and waits for it to exit. It is idempotent and safe
// from any goroutine. A loop ended by Stop (or by its context) returns nil;
// a loop that failed on its own returns that error.
func (m *MusicOnHold) Stop() error {
	m.cancel()
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if errors.Is(m.err, context.Canceled) {
		return nil
	}
	return m.err
}

func (m *MusicOnHold) finish(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

type mohConfig struct {
	tone audio.Tone
}

// MoHOption tunes DialogMedia.PlayMusicOnHold.
type MoHOption func(*mohConfig) error

// WithMoHTone sets the hold music source, overriding the dialog-level
// MediaConfig.MusicOnHold default for this loop.
func WithMoHTone(tone audio.Tone) MoHOption {
	return func(c *mohConfig) error {
		c.tone = tone
		return nil
	}
}

// PlayMusicOnHold starts looping hold music on the dialog and returns
// immediately. The tone source is the WithMoHTone option, falling back to the
// dialog-level MediaConfig.MusicOnHold; with neither configured it returns
// ErrMusicOnHoldNoTone. A dialog runs at most one hold-music loop: starting a
// second one returns ErrMusicOnHoldActive. Media setup errors (not answered,
// closed) are returned synchronously.
//
// The loop runs until Stop, StopMusicOnHold, ctx cancellation, or the dialog
// media closing; ctx cancellation surfaces through Stop as nil. While the
// peer holds us (negotiated recvonly/inactive) the RTP direction gate drops
// the audio — a warning is logged and the loop keeps running so an Unhold
// on either side resumes audibly.
func (d *DialogMedia) PlayMusicOnHold(ctx context.Context, opts ...MoHOption) (*MusicOnHold, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := mohConfig{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return nil, err
		}
	}
	tone := cfg.tone
	if len(tone.Segments) == 0 {
		d.mu.Lock()
		tone = d.mohTone
		d.mu.Unlock()
	}
	if len(tone.Segments) == 0 {
		return nil, ErrMusicOnHoldNoTone
	}
	return d.playMusicOnHold(ctx, tone, false)
}

// StopMusicOnHold stops the running hold-music loop, if any, and waits for it
// to exit. It is a no-op returning nil when nothing is playing.
func (d *DialogMedia) StopMusicOnHold() error {
	d.mu.Lock()
	m := d.moh
	d.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.Stop()
}

// playMusicOnHold installs the handle and spawns the loop. tone must carry at
// least one segment.
func (d *DialogMedia) playMusicOnHold(ctx context.Context, tone audio.Tone, auto bool) (*MusicOnHold, error) {
	// Synchronous render validation: resolve the writer now so setup mistakes
	// surface as a return value instead of a loop that dies instantly.
	props := MediaProps{}
	if _, err := d.audioWriterProps(&props); err != nil {
		return nil, err
	}

	m := &MusicOnHold{auto: auto, done: make(chan struct{})}
	m.ctx, m.cancel = context.WithCancel(ctx)

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		m.cancel()
		return nil, ErrDialogClosed
	}
	if d.moh != nil {
		d.mu.Unlock()
		m.cancel()
		return nil, ErrMusicOnHoldActive
	}
	// The negotiated direction is only mutated under d.mu (all RemoteSDP and
	// install paths hold it), so the read is race free here.
	dir := ""
	if d.mediaSession != nil {
		dir = d.mediaSession.NegotiatedDirection()
	}
	d.moh = m
	d.mu.Unlock()

	if dir == sdp.ModeRecvonly || dir == sdp.ModeInactive {
		slog.Warn("Music on hold started while the negotiated direction prevents sending; audio is dropped until the dialog is unheld", "direction", dir)
	}

	go d.mohLoop(m, tone, m.ctx)
	return m, nil
}

// mohAutoStart starts the automatic hold music after a successful Hold
// re-INVITE. toneOverride carries the per-call WithMusicOnHold choice; nil
// falls back to the dialog-level default. Best effort on purpose: the
// re-INVITE already succeeded, and surfacing a music failure as a Hold error
// would invite a retry straight into 491 glare. A loop that is already
// running (manual or previous Hold) is left untouched and unlogged.
func (d *DialogMedia) mohAutoStart(ctx context.Context, toneOverride *audio.Tone) {
	tone := audio.Tone{}
	if toneOverride != nil {
		tone = *toneOverride
	} else {
		d.mu.Lock()
		tone = d.mohTone
		d.mu.Unlock()
	}
	if len(tone.Segments) == 0 {
		return
	}
	if _, err := d.playMusicOnHold(ctx, tone, true); err != nil && !errors.Is(err, ErrMusicOnHoldActive) {
		slog.Warn("Hold: automatic music on hold failed to start", "error", err)
	}
}

// mohAutoStop stops the loop a Hold started automatically; manually started
// music is left running and stays under the caller's Stop/ctx control.
func (d *DialogMedia) mohAutoStop() {
	d.mu.Lock()
	m := d.moh
	d.mu.Unlock()
	if m == nil || !m.auto {
		return
	}
	if err := m.Stop(); err != nil {
		slog.Debug("Unhold: stopping automatic music on hold failed", "error", err)
	}
}

// mohLoop streams the tone into the dialog audio writer until ctx is done.
// Writer and codec are re-resolved every frame: a re-INVITE that changes the
// negotiated codec is picked up on the next frame by re-rendering the tone at
// the new sample rate — a Tone needs no resampler, unlike recorded sources.
//
// Lock order: the loop takes d.mu per frame via audioWriterProps. Nothing may
// hold d.mu while waiting on this loop; DialogMedia.Close only cancels.
func (d *DialogMedia) mohLoop(m *MusicOnHold, tone audio.Tone, ctx context.Context) {
	defer func() {
		d.mu.Lock()
		if d.moh == m {
			d.moh = nil
		}
		d.mu.Unlock()
		close(m.done)
	}()

	var (
		enc     audio.PCMEncoderWriter
		encW    io.Writer
		codec   media.Codec
		tr      *audio.ToneReader
		buf     []byte
		started bool
	)
	for {
		if err := ctx.Err(); err != nil {
			m.finish(err)
			return
		}

		props := MediaProps{}
		w, err := d.audioWriterProps(&props)
		if err != nil {
			// ErrDialogClosed after Close; ErrDialogNotAnswered should not
			// happen post-setup. Both are terminal for the loop.
			m.finish(err)
			return
		}

		if !started || w != encW || props.Codec != codec {
			enc = audio.PCMEncoderWriter{}
			if err := enc.Init(props.Codec, w); err != nil {
				m.finish(fmt.Errorf("music on hold: %w", err))
				return
			}
			tr = audio.NewToneReader(tone, int(props.Codec.SampleRate), props.Codec.NumChannels)
			tr.Loop()
			buf = make([]byte, props.Codec.Samples16())
			encW, codec, started = w, props.Codec, true
		}

		if _, rerr := io.ReadFull(tr, buf); rerr != nil {
			// A looping reader with a non-empty tone never returns EOF; a
			// partial/short read is terminal rather than silently lossy.
			m.finish(fmt.Errorf("music on hold: %w", rerr))
			return
		}
		if err := writeToneFrame(ctx, &enc, buf); err != nil {
			m.finish(err)
			return
		}
	}
}
