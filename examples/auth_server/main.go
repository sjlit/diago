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
)

// Dial this app with digest auth enabled, e.g.
// gophone dial -media=speaker -authuser=alice -authpass=wonderland "sip:123@127.0.0.1"
//
// Calls without (correct) credentials receive 401 Unauthorized with a
// WWW-Authenticate challenge and never reach the answer path.
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
	ua, _ := sipgo.NewUA()
	dg := diago.NewDiago(ua)

	// Nonce store for the digest challenges. Close it on shutdown.
	authServer := diago.NewDigestServer()
	defer authServer.Close()

	auth := diago.DigestAuth{
		Username: "alice",
		Password: "wonderland",
		Realm:    "example.pbx",
	}

	return dg.Serve(ctx, func(inDialog *diago.DialogServerSession) {
		slog.Info("New dialog request", "id", inDialog.ID)
		defer slog.Info("Dialog finished", "id", inDialog.ID)

		// Challenge/validate the INVITE. On first INVITE this answers 401 and
		// returns an error - the caller must re-INVITE with Authorization.
		if err := inDialog.Authorize(authServer, auth); err != nil {
			slog.Info("Call not authorized", "id", inDialog.ID, "error", err)
			return
		}

		inDialog.Trying()
		if err := inDialog.Answer(); err != nil {
			slog.Error("Answer failed", "error", err)
			return
		}
		slog.Info("Call authorized and answered", "id", inDialog.ID)

		<-inDialog.Context().Done()
	})
}
