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
	"time"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/examples"
)

// Run app:
// go run . sip:user@myregistrar.com

func main() {
	fUsername := flag.String("username", "", "Digest username")
	fPassword := flag.String("password", "", "Digest password")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -username <username> -password <pass> sip:123@example.com\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Setup signaling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	examples.SetupLogger()

	recipientUri := flag.Arg(0)
	if recipientUri == "" {
		flag.Usage()
		return
	}

	var regOpts []diago.RegisterOptions
	if *fUsername != "" || *fPassword != "" {
		regOpts = append(regOpts, diago.RegisterOptions{Username: *fUsername, Password: *fPassword})
	}
	err := start(ctx, recipientUri, *fUsername, regOpts...)
	if err != nil {
		slog.Error("PBX finished with error", "error", err)
	}
}

func start(ctx context.Context, recipientURI string, username string, regOpts ...diago.RegisterOptions) error {
	// Setup our main transaction user
	useragent := username
	if useragent == "" {
		useragent = "change-me"
	}

	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent(useragent),
		sipgo.WithUserAgentHostname("localhost"),
	)
	defer ua.Close()

	tu, err := diago.New(diago.Config{
		UserAgent: ua,
		Transports: []diago.Transport{{
			Network:  "udp",
			BindHost: "127.0.0.1",
			BindPort: 15060,
		}},
	})
	if err != nil {
		return err
	}

	// Start listening incoming calls
	go func() {
		tu.Serve(ctx, func(_ context.Context, inDialog *diago.InboundCall) error {
			slog.Info("New dialog request", "id", inDialog.ID())
			defer slog.Info("Dialog finished", "id", inDialog.ID())
			return nil
		})
	}()

	// Start registration and keep it running until the process is stopped.
	reg, err := tu.StartRegistration(ctx, recipientURI, regOpts...)
	if err != nil {
		return err
	}
	if err := reg.WaitReady(ctx); err != nil {
		return err
	}
	slog.Info("Registered", "recipient", recipientURI)

	<-ctx.Done()

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	return reg.Close(closeCtx)
}
