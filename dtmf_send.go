// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"time"
	"unicode"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

// DTMFMethod selects how SendDTMF delivers digits. SIP INFO (RFC 2926) is
// not implemented; DTMFMethodAuto falls back to inband audio tones when the
// peer has no telephone-event.
type DTMFMethod uint8

const (
	DTMFMethodAuto DTMFMethod = iota
	DTMFMethodRTP
	DTMFMethodInband
)

const (
	defaultDTMFInterval      = 80 * time.Millisecond
	defaultDTMFEventDuration = 80 * time.Millisecond
)

type dtmfSendConfig struct {
	method    DTMFMethod
	interval  time.Duration
	eventDur  time.Duration
	volume    uint8
	volumeSet bool
}

// DTMFSendOption tunes DialogMedia.SendDTMF.
type DTMFSendOption func(*dtmfSendConfig) error

// WithDTMFMethod selects the delivery method. Default DTMFMethodAuto:
// RFC 4733 when telephone-event is negotiated, inband dual tone otherwise.
func WithDTMFMethod(m DTMFMethod) DTMFSendOption {
	return func(c *dtmfSendConfig) error {
		switch m {
		case DTMFMethodAuto, DTMFMethodRTP, DTMFMethodInband:
			c.method = m
			return nil
		default:
			return fmt.Errorf("WithDTMFMethod: unknown method %d", m)
		}
	}
}

// WithDTMFInterval sets the silence between digits (default 80ms).
func WithDTMFInterval(d time.Duration) DTMFSendOption {
	return func(c *dtmfSendConfig) error {
		if d < 0 {
			return fmt.Errorf("WithDTMFInterval: negative duration")
		}
		c.interval = d
		return nil
	}
}

// WithDTMFEventDuration sets the per-digit event hold (RFC 4733 Duration
// field) and the inband tone length (default 80ms).
func WithDTMFEventDuration(d time.Duration) DTMFSendOption {
	return func(c *dtmfSendConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithDTMFEventDuration: must be greater than 0")
		}
		c.eventDur = d
		return nil
	}
}

// WithDTMFVolume sets the RFC 4733 signal volume, 0-63 relative dBov
// (default 10 when the option is not given; 0 is the loudest). Ignored by the
// inband method (tone level is fixed by the audio engine; use PlayTone with
// WithToneVolume for custom levels).
func WithDTMFVolume(v uint8) DTMFSendOption {
	return func(c *dtmfSendConfig) error {
		if v > 63 {
			return fmt.Errorf("WithDTMFVolume: %d out of range 0-63", v)
		}
		c.volume = v
		c.volumeSet = true
		return nil
	}
}

// SendDTMF sends a digit string ('0'-'9', 'A'-'D' case-insensitive, '*', '#')
// on the answered dialog. It blocks for roughly (event duration + interval)
// per digit. The context is honored between digits only — an in-flight event
// completes first, so cancellation latency is bounded by one event.
//
// The media pipeline is resolved at use time per docs/contracts.md §4:
// auto mode picks RFC 4733 on the negotiated telephone-event payload type,
// and falls back to inband dual tones when the peer does not support it.
// Explicit DTMFMethodRTP without negotiation returns ErrDTMFUnsupported.
//
// SIP INFO DTMF is NOT yet supported as a method.
func (m *DialogMedia) SendDTMF(ctx context.Context, digits string, opts ...DTMFSendOption) error {
	cfg := dtmfSendConfig{
		method:   DTMFMethodAuto,
		interval: defaultDTMFInterval,
		eventDur: defaultDTMFEventDuration,
	}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return err
		}
	}

	if digits == "" {
		return nil
	}
	for _, c := range digits {
		if !media.IsDTMFEvent(unicode.ToUpper(c)) {
			return fmt.Errorf("send dtmf: invalid digit %q", c)
		}
	}

	method := cfg.method
	if method == DTMFMethodAuto {
		negotiated, err := m.dtmfNegotiated()
		if err != nil {
			return err
		}
		if !negotiated {
			method = DTMFMethodInband
		} else {
			method = DTMFMethodRTP
		}
	}

	if method == DTMFMethodInband {
		segs := make([]audio.ToneSegment, 0, 2*len(digits))
		for _, d := range digits {
			tone, err := audio.ToneDTMFDigit(unicode.ToUpper(d))
			if err != nil {
				return err
			}
			seg := tone.Segments[0]
			seg.On = cfg.eventDur
			segs = append(segs, seg, audio.ToneSegment{Off: cfg.interval})
		}
		return m.PlayTone(ctx, audio.Tone{Segments: segs})
	}

	for _, d := range digits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.sendDTMFRTP(unicode.ToUpper(d), cfg); err != nil {
			return err
		}
		if err := waitDTMFContext(ctx, cfg.interval); err != nil {
			return err
		}
	}
	return nil
}

// dtmfNegotiated reports whether the current media session carries a
// telephone-event codec.
func (m *DialogMedia) dtmfNegotiated() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mediaGuard(); err != nil {
		return false, err
	}
	_, ok := m.mediaSession.DTMFCodecNegotiated()
	return ok, nil
}

// sendDTMFRTP resolves the negotiated telephone-event codec and the stable
// write handle under the dialog lock, then writes one RFC 4733 event outside
// the lock. The event itself is paced over ~7 * codec.SampleDur of real time
// (one packet per sample interval), so holding the lock across the write would
// stall dialog control operations (re-INVITE, Close, hold) for every digit.
// Handles are re-resolved per digit (contracts §4), so a mid-string re-INVITE
// changing the payload type or swapping the RTP session is picked up on the
// next digit. RTPPacketWriter is a stable handle — re-INVITE updates its
// internals via UpdateRTPSession rather than replacing the pointer — so
// capturing it and writing after unlock is safe; it serializes on its own
// internal lock against any concurrent write path.
func (m *DialogMedia) sendDTMFRTP(digit rune, cfg dtmfSendConfig) error {
	w, err := m.dtmfWriterForDigit()
	if err != nil {
		return err
	}
	return w.WriteDTMFWithOptions(digit, media.DTMFEncodeOptions{
		Volume:        cfg.volume,
		VolumeSet:     cfg.volumeSet,
		EventDuration: cfg.eventDur,
	})
}

// dtmfWriterForDigit snapshots the current DTMF write path under the dialog
// lock. It returns a writer whose packet handle stays valid after unlock; the
// passthrough writer is unused by the event path, so it is not captured.
func (m *DialogMedia) dtmfWriterForDigit() (*media.RTPDtmfWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mediaGuard(); err != nil {
		return nil, err
	}
	codec, ok := m.mediaSession.DTMFCodecNegotiated()
	if !ok {
		return nil, ErrDTMFUnsupported
	}
	if m.RTPPacketWriter == nil {
		return nil, ErrDialogNotAnswered
	}
	return media.NewRTPDTMFWriter(codec, m.RTPPacketWriter, nil), nil
}

func waitDTMFContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
