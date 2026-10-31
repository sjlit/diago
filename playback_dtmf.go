// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjlit/diago/v2/media"
)

var (
	// dtmfChSize is DTMF channel buffer size
	dtmfChSize = 16
)

// Playback streams audio into a dialog with pause, stop, resume, replay and
// mute controls, plus optional in-band RTP DTMF handling.
//
// Create it with Media.NewPlayback. With no PlaybackOptions it streams without
// DTMF; passing options enables DTMF interruption/replay and delivers received
// keys through DTMF().
type Playback struct {
	*playbackControl

	// nil interruptKeys means any key interrupts
	interruptKeys map[rune]struct{}
	replayKeys    map[rune]struct{}
	onDTMF        func(dtmf rune)

	dtmfCh     chan rune
	dtmfReader *DTMFReader
	// dm resolves the stable read handle at use time so the read loop
	// survives media updates (re-INVITE)
	dm *Media

	// readCtx is canceled by Close to interrupt the read loop through the
	// reader gate (no conn deadlines involved).
	readCtx    context.Context
	readCancel context.CancelFunc

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	started   atomic.Bool
}

// logger returns the dialog logger with the media package default as
// fallback (manually constructed handles may not carry a dialog).
func (p *Playback) logger() *slog.Logger {
	if p.dm != nil {
		return p.dm.logger()
	}
	return media.DefaultLogger()
}

// PlaybackOptions configures Media.NewPlayback.
type PlaybackOptions struct {
	// InterruptKeys lists DTMF keys that interrupt playback. Empty keeps the
	// default (any key interrupts) when DTMF control is enabled.
	InterruptKeys string
	// DisableInterrupt disables DTMF interruption entirely.
	DisableInterrupt bool
	// ReplayKeys lists DTMF keys that replay playback from the beginning.
	ReplayKeys string
	// OnDTMF is invoked for every received DTMF digit. It runs on the RTP
	// reading goroutine and must not block.
	OnDTMF func(rune)
}

// NewPlayback creates the unified playback handle. With no options it streams
// without DTMF control. Passing options enables DTMF interruption/replay.
//
// Nothing is streamed until one of the Play methods is called.
func (d *Media) NewPlayback(opts ...PlaybackOptions) (*Playback, error) {
	if len(opts) > 1 {
		return nil, fmt.Errorf("%w: NewPlayback accepts at most one PlaybackOptions", ErrInvalidOption)
	}
	control, err := d.newPlaybackControl()
	if err != nil {
		return nil, err
	}

	p := &Playback{
		playbackControl: control,
		dtmfCh:          make(chan rune, dtmfChSize),
		dm:              d,
	}
	if len(opts) == 0 {
		return p, nil
	}

	dtmfReader, err := d.NewDTMFReader()
	if err != nil {
		return nil, err
	}
	readCtx, readCancel := context.WithCancel(context.Background())
	p.dtmfReader = dtmfReader
	p.readCtx = readCtx
	p.readCancel = readCancel
	p.stopCh = make(chan struct{})

	o := opts[0]
	if o.DisableInterrupt {
		p.interruptKeys = map[rune]struct{}{}
	} else if o.InterruptKeys != "" {
		p.interruptKeys = parseDTMFKeys(o.InterruptKeys)
	}
	if o.ReplayKeys != "" {
		p.replayKeys = parseDTMFKeys(o.ReplayKeys)
	}
	if o.OnDTMF != nil {
		p.onDTMF = o.OnDTMF
	}
	dtmfReader.OnDTMF(p.handleDTMF)
	return p, nil
}

// DTMF returns channel of DTMF keys received during playback. Channel has
// buffer, on overflow keys are dropped.
func (p *Playback) DTMF() <-chan rune {
	return p.dtmfCh
}

// PlayContext plays reader content with DTMF control, stopping with ctx.Err()
// when the context is canceled. DTMF reading continues until Close.
func (p *Playback) PlayContext(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	p.startReadLoop()
	return p.playbackControl.PlayContext(ctx, reader, mimeType)
}

// PlayFileContext plays wav file with DTMF control, stopping with ctx.Err()
// when the context is canceled.
func (p *Playback) PlayFileContext(ctx context.Context, filename string) (int64, error) {
	p.startReadLoop()
	return p.playbackControl.PlayFileContext(ctx, filename)
}

// PlayURLContext plays wav from url with DTMF control, stopping with
// ctx.Err() when the context is canceled.
func (p *Playback) PlayURLContext(ctx context.Context, urlStr string) (int64, error) {
	p.startReadLoop()
	return p.playbackControl.PlayURLContext(ctx, urlStr)
}

// PlayReader plays reader content. An empty mimeType streams it as raw PCM.
func (p *Playback) PlayReader(ctx context.Context, reader io.Reader, mimeType string) error {
	_, err := p.PlayContext(ctx, reader, mimeType)
	return err
}

// PlayFile plays a WAV file.
func (p *Playback) PlayFile(ctx context.Context, filename string) error {
	_, err := p.PlayFileContext(ctx, filename)
	return err
}

// PlayURL plays WAV content from a URL.
func (p *Playback) PlayURL(ctx context.Context, urlStr string) error {
	_, err := p.PlayURLContext(ctx, urlStr)
	return err
}

// Close stops playback and DTMF reading, then waits for the read loop to
// exit. It is idempotent. Cancellation goes through the reader gate (no conn
// deadlines): the in-flight read is interrupted and the reader is restored
// afterwards.
func (p *Playback) Close() error {
	p.closeOnce.Do(func() {
		if p.playbackControl != nil {
			p.playbackControl.Stop()
		}
		if p.stopCh != nil {
			close(p.stopCh)
		}
		if p.readCancel != nil {
			p.readCancel()
		}
		p.wg.Wait()
	})
	return nil
}

// startReadLoop starts DTMF reading in background. It is idempotent.
func (p *Playback) startReadLoop() {
	if p.dtmfReader == nil {
		return
	}
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	p.wg.Add(1)
	go p.readLoop()
}

func (p *Playback) readLoop() {
	defer p.wg.Done()

	buf := make([]byte, media.RTPBufSize)

	// Arm a ctx-driven interrupt on the stable read handle: Close cancels
	// the context, which unblocks the in-flight read - no conn deadlines.
	if p.dm != nil {
		if pr := p.dm.currentReadHandle(); pr != nil {
			disarm := pr.ArmReadInterrupt(p.readCtx)
			defer disarm()
		}
	}

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		_, err := p.dtmfReader.Read(buf)
		if err != nil {
			switch {
			case p.readCtx != nil && p.readCtx.Err() != nil:
				// Close initiated the stop
				return
			case errors.Is(err, media.ErrReadPaused):
				// External pause: wait it out, the loop resumes after release
				time.Sleep(5 * time.Millisecond)
				continue
			case errors.Is(err, os.ErrDeadlineExceeded):
				// Legacy deadline users
				continue
			}
			// Dialog closed (net closed => io.EOF) or reading stopped
			p.logger().Debug("DTMF read loop stopped", "error", err)
			return
		}
	}
}

// handleDTMF routes DTMF to playback actions and DTMF channel.
// It is executed on RTP reading goroutine and MUST NOT block.
func (p *Playback) handleDTMF(dtmf rune) error {
	replayed := false
	if _, ok := p.replayKeys[dtmf]; ok {
		if err := p.control.Replay(); err != nil {
			p.logger().Debug("DTMF replay skipped", "dtmf", string(dtmf), "error", err)
		} else {
			replayed = true
		}
	}

	if !replayed && p.interruptKeys == nil {
		// Default any key interrupts
		p.control.Stop()
	} else if !replayed {
		if _, ok := p.interruptKeys[dtmf]; ok {
			p.control.Stop()
		}
	}

	select {
	case p.dtmfCh <- dtmf:
	default:
		p.logger().Warn("DTMF dropped, DTMF channel is full", "dtmf", string(dtmf))
	}

	if p.onDTMF != nil {
		p.onDTMF(dtmf)
	}
	return nil
}

func parseDTMFKeys(keys string) map[rune]struct{} {
	m := make(map[rune]struct{}, len(keys))
	for _, r := range keys {
		m[r] = struct{}{}
	}
	return m
}
