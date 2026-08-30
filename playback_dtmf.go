// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjlit/diago/media"
)

var (
	// dtmfChSize is DTMF channel buffer size
	dtmfChSize = 16
)

// PlaybackDTMFOption configures AudioPlaybackDTMF
type PlaybackDTMFOption func(*AudioPlaybackDTMF)

// WithInterruptKeys sets DTMF keys that interrupt playback. Default is any
// key interrupts. Empty string disables interrupting.
func WithInterruptKeys(keys string) PlaybackDTMFOption {
	return func(p *AudioPlaybackDTMF) {
		p.interruptKeys = parseDTMFKeys(keys)
	}
}

// WithReplayKeys sets DTMF keys that replay playback from the beginning.
// Default is no replay keys.
func WithReplayKeys(keys string) PlaybackDTMFOption {
	return func(p *AudioPlaybackDTMF) {
		p.replayKeys = parseDTMFKeys(keys)
	}
}

// WithOnDTMF registers additional callback invoked on every received DTMF.
// It is executed on RTP reading goroutine and MUST NOT block.
func WithOnDTMF(fn func(dtmf rune)) PlaybackDTMFOption {
	return func(p *AudioPlaybackDTMF) {
		p.onDTMF = fn
	}
}

// AudioPlaybackDTMF is playback that can be interrupted or replayed with
// in-band RTP DTMF (telephone-event).
//
// All received DTMF keys are delivered to DTMF() channel, so playback decision
// like "which key caller pressed" can be done after play.
//
// PlaybackControl (Stop, Pause, Resume, Replay, Mute) can be used in parallel.
type AudioPlaybackDTMF struct {
	AudioPlaybackControl

	// nil interruptKeys means any key interrupts
	interruptKeys map[rune]struct{}
	replayKeys    map[rune]struct{}
	onDTMF        func(dtmf rune)

	dtmfCh     chan rune
	dtmfReader *DTMFReader
	// dm resolves the stable read handle at use time so the read loop
	// survives media updates (re-INVITE) - docs/contracts.md §4
	dm *DialogMedia

	// readCtx is canceled by Close to interrupt the read loop through the
	// reader gate (no conn deadlines involved).
	readCtx    context.Context
	readCancel context.CancelFunc

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	started   atomic.Bool
}

// PlaybackDTMFCreate creates playback controlled with in-band RTP DTMF.
//
// By default any DTMF key interrupts playback. Use options to customize:
//
//	pb, _ := dialog.PlaybackDTMFCreate(
//	    WithInterruptKeys("1234567890"), // only number keys interrupt
//	    WithReplayKeys("*"),             // star key replays prompt
//	)
//	go pb.PlayFile("menu.wav")
//	dtmf := <-pb.DTMF()
//	pb.Close()
//
// While playback is active DTMF keys are detected by reading audio RTP in
// background. Other audio reading (AudioReaderDTMF, Echo, recording) MUST NOT
// be used until Close is called.
func (d *DialogMedia) PlaybackDTMFCreate(opts ...PlaybackDTMFOption) (*AudioPlaybackDTMF, error) {
	pb, err := d.PlaybackControlCreate()
	if err != nil {
		return nil, err
	}

	dtmfReader, err := d.AudioReaderDTMF()
	if err != nil {
		return nil, err
	}

	readCtx, readCancel := context.WithCancel(context.Background())
	p := &AudioPlaybackDTMF{
		AudioPlaybackControl: pb,
		dtmfCh:               make(chan rune, dtmfChSize),
		dtmfReader:           dtmfReader,
		dm:                   d,
		readCtx:              readCtx,
		readCancel:           readCancel,
		stopCh:               make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}

	dtmfReader.OnDTMF(p.handleDTMF)
	return p, nil
}

// DTMF returns channel of DTMF keys received during playback. Channel has
// buffer, on overflow keys are dropped.
func (p *AudioPlaybackDTMF) DTMF() <-chan rune {
	return p.dtmfCh
}

// Play plays reader content with DTMF control.
//
// Deprecated: Use PlayContext.
func (p *AudioPlaybackDTMF) Play(reader io.Reader, mimeType string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.Play(reader, mimeType)
}

// PlayContext plays reader content with DTMF control, stopping with ctx.Err()
// when the context is canceled. DTMF reading continues until Close.
func (p *AudioPlaybackDTMF) PlayContext(ctx context.Context, reader io.Reader, mimeType string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayContext(ctx, reader, mimeType)
}

// PlayFile plays wav file with DTMF control.
//
// Deprecated: Use PlayFileContext.
func (p *AudioPlaybackDTMF) PlayFile(filename string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayFile(filename)
}

// PlayFileContext plays wav file with DTMF control, stopping with ctx.Err()
// when the context is canceled.
func (p *AudioPlaybackDTMF) PlayFileContext(ctx context.Context, filename string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayFileContext(ctx, filename)
}

// PlayURL plays wav from url with DTMF control.
//
// Deprecated: Use PlayURLContext.
func (p *AudioPlaybackDTMF) PlayURL(urlStr string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayURL(urlStr)
}

// PlayURLContext plays wav from url with DTMF control, stopping with
// ctx.Err() when the context is canceled.
func (p *AudioPlaybackDTMF) PlayURLContext(ctx context.Context, urlStr string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayURLContext(ctx, urlStr)
}

// Close stops DTMF reading. It should be called when DTMF playback is not
// needed anymore, to allow other audio reading on the dialog.
//
// Cancellation goes through the reader gate (no conn deadlines): the
// in-flight read is interrupted and the reader is restored afterwards.
// dm may be nil when the struct is constructed manually (test stubs); the
// read loop simply has no media to arm in that case.
func (p *AudioPlaybackDTMF) Close() error {
	p.closeOnce.Do(func() {
		close(p.stopCh)
		if p.readCancel != nil {
			p.readCancel()
		}
		p.wg.Wait()
	})
	return nil
}

// startReadLoop starts DTMF reading in background. It is idempotent.
func (p *AudioPlaybackDTMF) startReadLoop() {
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	p.wg.Add(1)
	go p.readLoop()
}

func (p *AudioPlaybackDTMF) readLoop() {
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
			slog.Debug("DTMF read loop stopped", "error", err)
			return
		}
	}
}

// handleDTMF routes DTMF to playback actions and DTMF channel.
// It is executed on RTP reading goroutine and MUST NOT block.
func (p *AudioPlaybackDTMF) handleDTMF(dtmf rune) error {
	replayed := false
	if _, ok := p.replayKeys[dtmf]; ok {
		if err := p.control.Replay(); err != nil {
			slog.Debug("DTMF replay skipped", "dtmf", string(dtmf), "error", err)
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
		slog.Warn("DTMF dropped, DTMF channel is full", "dtmf", string(dtmf))
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
