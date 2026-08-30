// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/sjlit/diago/audio"
	"github.com/sjlit/diago/media"
)

// AudioRingtone is playback for ringtone
type AudioRingtone struct {
	writer     *audio.PCMEncoderWriter
	ringtone   []byte
	sampleSize int
	// dm resolves the stable write handle at use time; stop goes through the
	// write gate (docs/contracts.md §4)
	dm *DialogMedia
}

func (a *AudioRingtone) PlayBackground() (func() error, error) {
	if err := a.dm.checkMediaUsable(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	wg := sync.WaitGroup{}
	wg.Add(1)
	var playErr error
	go func() {
		defer wg.Done()
		playErr = a.play(ctx)
	}()

	return func() error {
		cancel()

		// Stop the play loop through the write gate: the in-flight write
		// finishes (bounded by one packet interval) and the next write
		// surfaces media.ErrWritePaused.
		release, err := a.dm.PauseAudioWrite()
		if err != nil {
			return err
		}
		wg.Wait()
		release()

		if errors.Is(playErr, media.ErrWritePaused) {
			return nil
		}
		if e, ok := playErr.(net.Error); ok && e.Timeout() {
			return nil
		}
		return playErr
	}, nil
}

func (a *AudioRingtone) Play(ctx context.Context) error {
	return a.play(ctx)
}

func (a *AudioRingtone) play(timerCtx context.Context) error {
	t := time.NewTimer(0)
	for {
		_, err := media.WriteAll(a.writer, a.ringtone, a.sampleSize)
		if err != nil {
			return err
		}

		t.Reset(4 * time.Second)
		select {
		case <-t.C:
		case <-timerCtx.Done():
			return timerCtx.Err()
		}
	}
}
