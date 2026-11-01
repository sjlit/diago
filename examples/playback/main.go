// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/testdata"
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
	tu, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		return err
	}

	return tu.Serve(ctx, func(ctx context.Context, inDialog *diago.InboundCall) error {
		slog.Info("New dialog request", "id", inDialog.ID())
		defer slog.Info("Dialog finished", "id", inDialog.ID())
		if err := Playback(ctx, inDialog); err != nil {
			slog.Error("Failed to play", "error", err)
		}
		return nil
	})
}

func Playback(ctx context.Context, inDialog *diago.InboundCall) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing
	if err := inDialog.Answer(ctx); err != nil {
		return err
	} // Answer -> 200 Response

	playfile, err := testdata.OpenFile("demo-echodone.wav")
	if err != nil {
		return err
	}
	defer playfile.Close()
	slog.Info("Playing a file", "file", "demo-echodone.wav")

	pb, err := inDialog.NewPlayback()
	if err != nil {
		return err
	}
	defer pb.Close()
	_, err = pb.PlayContext(ctx, playfile, "audio/wav")
	if err != nil {
		return err
	}
	// The call stays alive after the handler returns (v3 semantics); end it
	// after the announcement.
	if err := inDialog.Close(ctx); err != nil {
		slog.Error("Close failed", "error", err)
	}
	return nil
}
