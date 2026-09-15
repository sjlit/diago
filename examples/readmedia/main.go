// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/emiago/sipgo"
	"github.com/sjlit/diago/v2"
	"github.com/sjlit/diago/v2/audio"
	"github.com/sjlit/diago/v2/examples"
	"github.com/sjlit/diago/v2/media"
)

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
		ReadMedia(ctx, inDialog)
		return nil
	})
}

func ReadMedia(ctx context.Context, inDialog *diago.DialogServerSession) error {
	inDialog.Trying(ctx)  // 100 Trying
	inDialog.Ringing(ctx) // 180 Ringing
	if err := inDialog.Answer(ctx); err != nil {
		return err
	}

	lastPrint := time.Now()
	pktsCount := 0
	buf := make([]byte, media.RTPBufSize)

	// After answer we can access audio reader and read props
	audioReader, m, err := inDialog.ReaderWithProps()
	if err != nil {
		return err
	}

	decoder, err := audio.NewPCMDecoderReader(m.Codec.PayloadType, audioReader)
	if err != nil {
		return err
	}
	for {
		_, err := decoder.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		pkt := inDialog.RTPPacketReader().PacketHeader
		if time.Since(lastPrint) > 3*time.Second {
			lastPrint = time.Now()
			slog.Info("Received packets", "PayloadType", pkt.PayloadType, "pkts", pktsCount)
		}
		pktsCount++
	}
}
