// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
)

// Have receiver running:
// gophone answer -l "127.0.0.1:5090"
//
// Run app:
// go run . sip:uas@127.0.0.1:5090
//
// Run dialer:
// gophone dial sip:bob@127.0.0.1

var bridge *diago.BridgeMix

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	flag.Parse()
	bridge = diago.NewBridgeMix()

	err := start(ctx)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context) error {
	// Setup our main transaction user
	ua, _ := sipgo.NewUA()
	d, err := diago.NewDiago(ua, diago.WithDefaultTransport())
	if err != nil {
		return err
	}
	return d.Serve(ctx, func(ctx context.Context, inDialog *diago.DialogServerSession) error {
		BridgeCall(ctx, d, inDialog)

		fmt.Println("Bridge state", bridge.String())
		return nil
	})
}

func BridgeCall(ctx context.Context, d *diago.Diago, inDialog *diago.DialogServerSession) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing

	inCtx := inDialog.Context()

	if err := inDialog.Answer(ctx); err != nil {
		return err
	}
	if err := bridge.AddDialogSession(inDialog); err != nil {
		return err
	}

	<-inCtx.Done()
	return bridge.RemoveDialogSession(inDialog)
}
