// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago"
	"github.com/sjlit/diago/examples"
	"github.com/sjlit/diago/testdata"
)

// Dial this app with
// gophone dial -media=speaker "sip:123@127.0.0.1"

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
	tu, err := diago.NewDiago(ua, diago.WithDefaultTransport())
	if err != nil {
		return err
	}

	return tu.Serve(ctx, func(ctx context.Context, inDialog *diago.DialogServerSession) error {
		slog.Info("New dialog request", "id", inDialog.ID())
		defer slog.Info("Dialog finished", "id", inDialog.ID())
		if err := Playback(ctx, inDialog); err != nil {
			slog.Error("Failed to play", "error", err)
		}
		return nil
	})
}

func Playback(ctx context.Context, inDialog *diago.DialogServerSession) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing
	if err := inDialog.Answer(ctx); err != nil {
		return err
	} // Answer -> 200 Response

	playfile, _ := testdata.OpenFile("demo-echodone.wav")
	slog.Info("Playing a file", "file", "demo-echodone.wav")

	pb, err := inDialog.CreatePlayback()
	if err != nil {
		return err
	}
	_, err = pb.Play(playfile, "audio/wav")
	return err
}
