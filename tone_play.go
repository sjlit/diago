// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

type toneConfig struct {
	loop   bool
	volume float64
}

// ToneOption tunes DialogMedia.PlayTone.
type ToneOption func(*toneConfig) error

// WithToneLoop replays the tone until the context is canceled. Without it the
// tone plays once.
func WithToneLoop() ToneOption {
	return func(c *toneConfig) error {
		c.loop = true
		return nil
	}
}

// WithToneVolume scales every segment volume (0..1, values above 1 are
// clamped). Default tone volume is applied when segments set none.
func WithToneVolume(scale float64) ToneOption {
	return func(c *toneConfig) error {
		if scale <= 0 {
			return fmt.Errorf("WithToneVolume: scale must be greater than 0")
		}
		c.volume = scale
		return nil
	}
}

// PlayTone synthesizes a tone and streams it to the dialog's audio writer.
// It blocks until the tone finishes; with WithToneLoop it runs until ctx is
// canceled and then returns ctx.Err() (docs/contracts.md §9 cancellation
// style — cancellation latency is one packet interval).
//
// While the dialog's write gate is paused by another component
// (PauseAudioWrite), PlayTone waits for the gate to release instead of
// failing the tone; ctx cancellation still interrupts the wait.
//
// The tone is written into the audio pipeline regardless of the negotiated SDP
// direction: playing ringback to an answered originator works after Answer,
// and a peer that is sendonly simply ignores the packets.
func (d *DialogMedia) PlayTone(ctx context.Context, tone audio.Tone, opts ...ToneOption) error {
	cfg := toneConfig{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return err
		}
	}
	if cfg.volume > 0 {
		tone = tone.WithVolume(cfg.volume)
	}

	props := MediaProps{}
	w, err := d.audioWriterProps(&props)
	if err != nil {
		return err
	}

	enc := audio.PCMEncoderWriter{}
	if err := enc.Init(props.Codec, w); err != nil {
		return fmt.Errorf("play tone: %w", err)
	}

	tr := audio.NewToneReader(tone, int(props.Codec.SampleRate), props.Codec.NumChannels)
	if cfg.loop {
		tr.Loop()
	}

	buf := make([]byte, props.Codec.Samples16())
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, rerr := io.ReadFull(tr, buf)
		if n > 0 {
			if err := writeToneFrame(ctx, &enc, buf[:n]); err != nil {
				return err
			}
		}
		switch {
		case rerr == nil:
			continue
		case errors.Is(rerr, io.EOF), errors.Is(rerr, io.ErrUnexpectedEOF):
			return nil
		default:
			return rerr
		}
	}
}

// writeToneFrame writes one encoded frame, waiting out a concurrent
// PauseAudioWrite gate rather than failing the tone: a transient pause by
// another component must not cut the signal (or an inband DTMF digit) short.
// The frame is retried unchanged so no samples are lost; ctx still cancels.
func writeToneFrame(ctx context.Context, enc *audio.PCMEncoderWriter, frame []byte) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := enc.Write(frame)
		if !errors.Is(err, media.ErrWritePaused) {
			return err
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
