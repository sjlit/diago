// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
)

// Run the N-party mixer:
//
//	go run .
//
// Dial in from two or more softphones:
//
//	gophone dial -ua alice sip:112@127.0.0.1
//	gophone dial -ua bob sip:112@127.0.0.1

var bridge *diago.BridgeMix

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	bridge = diago.NewBridgeMix()

	err := start(ctx)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context) error {
	// Setup our main transaction user
	d, err := diago.New(diago.Config{
		Transports: []diago.Transport{diago.DefaultTransport()},
	})
	if err != nil {
		return err
	}
	return d.Serve(ctx, func(ctx context.Context, inDialog *diago.InboundCall) error {
		if err := BridgeCall(ctx, d, inDialog); err != nil {
			return err
		}

		fmt.Println("Bridge state", bridge.String())
		return nil
	})
}

func BridgeCall(ctx context.Context, d *diago.Agent, inDialog *diago.InboundCall) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing

	inCtx := inDialog.Context()

	if err := inDialog.Answer(ctx); err != nil {
		return err
	}
	if err := bridge.Add(inDialog); err != nil {
		return err
	}

	<-inCtx.Done()
	return bridge.Remove(inDialog)
}
