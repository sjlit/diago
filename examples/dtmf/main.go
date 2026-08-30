// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/sjlit/diago"
	"github.com/sjlit/diago/examples"
	"github.com/emiago/sipgo"
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
	tu := diago.NewDiago(ua)

	return tu.Serve(ctx, func(inDialog *diago.DialogServerSession) {
		slog.Info("New dialog request", "id", inDialog.ID)
		defer slog.Info("Dialog finished", "id", inDialog.ID)
		ReadDTMF(inDialog)
	})
}

func ReadDTMF(inDialog *diago.DialogServerSession) error {
	inDialog.Trying()  // Progress -> 100 Trying
	inDialog.Ringing() // Ringing -> 180 Response
	if err := inDialog.Answer(); err != nil {
		return err
	}
	slog.Info("Reading DTMF")

	reader, err := inDialog.AudioReaderDTMF()
	if err != nil {
		return err
	}
	listenCtx, cancel := context.WithTimeout(inDialog.Context(), 10*time.Second)
	defer cancel()
	return reader.ListenContext(listenCtx, func(dtmf rune) error {
		slog.Info("Received DTMF", "dtmf", string(dtmf))
		return nil
	})
}
