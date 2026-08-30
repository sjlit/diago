// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
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
	// dtmfReadPollInterval is read deadline used in DTMF read loop. It only
	// affects loop exit latency, DTMF packets are detected immediately.
	dtmfReadPollInterval = 500 * time.Millisecond
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
	// dm resolves the CURRENT media session at use time so the read loop
	// survives media updates (re-INVITE) - docs/contracts.md §4
	dm *DialogMedia

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

	p := &AudioPlaybackDTMF{
		AudioPlaybackControl: pb,
		dtmfCh:               make(chan rune, dtmfChSize),
		dtmfReader:           dtmfReader,
		dm:                   d,
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

// Play plays reader content with DTMF control. See AudioPlaybackControl.Play
func (p *AudioPlaybackDTMF) Play(reader io.Reader, mimeType string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.Play(reader, mimeType)
}

// PlayFile plays wav file with DTMF control. See AudioPlaybackControl.PlayFile
func (p *AudioPlaybackDTMF) PlayFile(filename string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayFile(filename)
}

// PlayURL plays wav from url with DTMF control. See AudioPlaybackControl.PlayURL
func (p *AudioPlaybackDTMF) PlayURL(urlStr string) (int64, error) {
	p.startReadLoop()
	return p.AudioPlaybackControl.PlayURL(urlStr)
}

// Close stops DTMF reading. It should be called when DTMF playback is not
// needed anymore, to allow other audio reading on the dialog.
//
// dm may be nil when the struct is constructed manually (test stubs); the
// deadline control is simply skipped in that case.
func (p *AudioPlaybackDTMF) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.stopCh)
		if p.dm != nil {
			if ms := p.dm.currentMediaSession(); ms != nil {
				// Unblock pending read
				err = ms.StopRTP(1, time.Millisecond)
			}
		}
		p.wg.Wait()
		if p.dm != nil {
			if ms := p.dm.currentMediaSession(); ms != nil {
				// Restore reading without deadline
				err = errors.Join(err, ms.StartRTP(1))
			}
		}
	})
	return err
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
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		// Resolve the current session each iteration: media updates (re-INVITE)
		// swap sessions, but RTP/RTCP share the same conn deadlines. dm may be
		// nil when constructed manually (test stubs).
		if p.dm != nil {
			if ms := p.dm.currentMediaSession(); ms != nil {
				// Use short read deadline, so loop can check stop channel.
				// Deadline does not add DTMF detection latency, since read
				// returns immediately on packet arrival.
				if err := ms.StopRTP(1, dtmfReadPollInterval); err != nil {
					slog.Debug("Failed to set DTMF read deadline", "error", err)
				}
			}
		}

		_, err := p.dtmfReader.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
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
