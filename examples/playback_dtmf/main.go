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
// gophone dial -media=audio "sip:123@127.0.0.1"
//
// While prompt is played:
//   - pressing * replays the prompt from the beginning
//   - pressing any other key interrupts the prompt and call is hanged up

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
	tu := diago.NewDiago(ua)

	return tu.Serve(ctx, func(inDialog *diago.DialogServerSession) {
		slog.Info("New dialog request", "id", inDialog.ID)
		defer slog.Info("Dialog finished", "id", inDialog.ID)

		if err := PlaybackDTMF(inDialog); err != nil {
			slog.Error("Playback finished with error", "error", err)
		}
	})
}

func PlaybackDTMF(inDialog *diago.DialogServerSession) error {
	inDialog.Trying()  // Progress -> 100 Trying
	inDialog.Ringing() // Ringing -> 180 Response
	if err := inDialog.Answer(); err != nil {
		return err
	}

	pb, err := inDialog.PlaybackDTMFCreate(
		diago.WithReplayKeys("*"),
	)
	if err != nil {
		return err
	}
	defer pb.Close()

	slog.Info("Playing prompt. Press * to replay, any other key to continue")

	go func() {
		playfile, _ := testdata.OpenFile("demo-echotest.wav")
		defer playfile.Close()

		if _, err := pb.PlayContext(inDialog.Context(), playfile, "audio/wav"); err != nil {
			slog.Error("Playing finished with error", "error", err)
		}
	}()

	// Wait which key caller pressed
	dtmf := <-pb.DTMF()
	slog.Info("Caller pressed", "dtmf", string(dtmf))

	if dtmf != '*' {
		slog.Info("Finishing call", "dtmf", string(dtmf))
		return inDialog.Hangup(context.TODO())
	}

	// Playback is being replayed with * key. Wait next caller decision
	slog.Info("Prompt replayed. Press any key to continue")
	dtmf = <-pb.DTMF()
	slog.Info("Caller pressed", "dtmf", string(dtmf))
	return inDialog.Hangup(context.TODO())
}
