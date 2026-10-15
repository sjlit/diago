// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/testdata"
)

// Dial this app with
// gophone dial -media=audio "sip:123@127.0.0.1"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	err := start(ctx)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context) error {
	// Setup our main transaction user
	ua, _ := sipgo.NewUA()
	tu, err := diago.New(diago.WithUserAgent(ua), diago.WithDefaultTransport())
	if err != nil {
		return err
	}

	return tu.Serve(ctx, func(ctx context.Context, inDialog *diago.InboundCall) error {
		slog.Info("New dialog request", "id", inDialog.ID())
		defer slog.Info("Dialog finished", "id", inDialog.ID())

		if err := Playback(ctx, inDialog); err != nil {
			slog.Error("Playback finished with error", "error", err)
		}
		return nil
	})
}

func Playback(ctx context.Context, inDialog *diago.InboundCall) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing

	playfile, _ := testdata.OpenFile("demo-echotest.wav")
	defer playfile.Close()

	slog.Info("Playing a file", "file", "demo-echotest.wav")

	if err := inDialog.Answer(ctx); err != nil {
		return err
	}

	pb, err := inDialog.NewPlaybackControl()
	if err != nil {
		return err
	}

	playFinished := make(chan error)
	go func() {
		_, err = pb.PlayContext(ctx, playfile, "audio/wav")
		playFinished <- err
	}()

	t1 := time.After(3 * time.Second)
	t2 := time.After(6 * time.Second)
	t3 := time.After(10 * time.Second)
	for {
		select {
		case <-t1:
			pb.Mute(true)
			slog.Info("Audio muted")
		case <-t2:
			pb.Mute(false)
			slog.Info("Audio unmuted")
		case <-t3:
			pb.Stop()
			slog.Info("Audio stopped")
		case err := <-playFinished:
			slog.Info("Play finished", "error", err)
			// The call stays alive after the handler returns (v3
			// semantics); end it after playback.
			if herr := inDialog.Hangup(ctx); herr != nil {
				slog.Error("Hangup failed", "error", herr)
			}
			return nil
		case <-inDialog.Context().Done():
			slog.Info("Call hanguped", "error", inDialog.Context().Err())
			return nil
		}
	}
}
