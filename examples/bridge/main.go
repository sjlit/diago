// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"time"

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

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	flag.Parse()
	recipientUri := flag.Arg(0)

	err := start(ctx, recipientUri)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context, recipientUri string) error {
	// Setup our main transaction user
	ua, _ := sipgo.NewUA()
	d, err := diago.New(diago.Config{
		UserAgent:  ua,
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		return err
	}

	return d.Serve(ctx, func(ctx context.Context, inDialog *diago.InboundCall) error {
		BridgeCall(ctx, d, inDialog, recipientUri)
		return nil
	})
}

func BridgeCall(ctx context.Context, d *diago.Agent, inDialog *diago.InboundCall, recipient string) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing

	inCtx := inDialog.Context()
	answerCtx, cancel := context.WithTimeout(inCtx, 5*time.Second)
	defer cancel()

	bridge := diago.NewBridge()
	// Now answer our in dialog
	if err := inDialog.Answer(answerCtx); err != nil {
		return err
	}
	if err := bridge.Add(inDialog); err != nil {
		return err
	}

	outDialog, err := d.DialBridge(answerCtx, recipient, bridge)
	if err != nil {
		return err
	}
	defer outDialog.Close()
	outCtx := outDialog.Context()

	defer inDialog.Hangup(inCtx)
	defer outDialog.Hangup(outCtx)

	// You can even easily detect who hangups
	select {
	case <-inCtx.Done():
	case <-outCtx.Done():
	}
	return nil
}
