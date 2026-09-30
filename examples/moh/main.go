// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/emiago/sipgo"

	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/examples"
)

// Example: Music on Hold.
//
// Run with:
//
//	go run ./examples/moh
//
// Dial:
//
//	sip:alice@127.0.0.1
//
// Once the call is up, press:
//
//	1 → Hold (auto MoH starts from MediaConfig.MusicOnHold)
//	2 → Unhold (auto MoH stops)
//
// For manual control:
//
//	go func() { time.Sleep(5*time.Second); inDialog.StartMusicOnHold(ctx, diago.WithMoHTone(myTone)) }()
//	go func() { time.Sleep(15*time.Second); inDialog.StopMusicOnHold() }()

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("PBX exited", "error", err)
	}
}

func run(ctx context.Context) error {
	ua, _ := sipgo.NewUA()
	defer ua.Close()

	// Dialog-level default hold music: a 425Hz hold tone at 30ms cadence.
	holdTone := audio.Tone{
		Segments: []audio.ToneSegment{{Freqs: []float64{425}, On: 30 * time.Millisecond}},
	}

	dg, err := diago.New(diago.WithUserAgent(ua),
		diago.WithTransport(diago.Transport{Network: "udp", BindHost: "127.0.0.1", BindPort: 5060}),
		diago.WithMediaConfig(diago.MediaConfig{MusicOnHold: holdTone}),
	)
	if err != nil {
		return err
	}

	return dg.Serve(ctx, func(ctx context.Context, inDialog *diago.InboundCall) error {
		if err := inDialog.Answer(ctx); err != nil {
			slog.Error("Answer failed", "error", err)
			return err
		}

		// DTMF reader for Hold (1) and Unhold (2).
		reader, err := inDialog.NewDTMFReader()
		if err != nil {
			slog.Error("NewDTMFReader failed", "error", err)
			return err
		}
		reader.OnDTMF(func(r rune) error {
			switch r {
			case '1':
				slog.Info("DTMF 1 → Hold (auto MoH)")
				if err := inDialog.Hold(ctx); err != nil {
					slog.Error("Hold failed", "error", err)
				}
			case '2':
				slog.Info("DTMF 2 → Unhold")
				if err := inDialog.Unhold(ctx); err != nil {
					slog.Error("Unhold failed", "error", err)
				}
			}
			return nil
		})

		// Block until the call ends; the framework releases the media and the
		// cache entry when it does.
		<-inDialog.Context().Done()
		return nil
	})
}
