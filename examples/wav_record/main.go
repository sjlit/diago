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

	"github.com/sjlit/diago"
	"github.com/sjlit/diago/examples"
	"github.com/sjlit/diago/media"
	"github.com/emiago/sipgo"
)

// Dial this app with
// gophone dial -media=audio "sip:123@127.0.0.1"

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
		if err := Record(inDialog); err != nil {
			slog.Error("Record finished with error", "error", err)
		}
	})
}

func Record(inDialog *diago.DialogServerSession) error {
	inDialog.Trying()  // Progress -> 100 Trying
	inDialog.Ringing() // Ringing -> 180 Response
	if err := inDialog.Answer(); err != nil {
		return err
	} // Answer -> 200 Response

	// Create wav file to store recording
	filename := "/tmp/diago_record_" + inDialog.InviteRequest.CallID().Value() + ".wav"
	slog.Info("Creating new recording", "filename", filename)
	wavFile, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer wavFile.Close()

	// Install a stereo recording tap into the audio pipeline. It must be
	// started before the dialog joins a Bridge (see docs/contracts.md §12).
	// The tap is fail-open by default: a full disk degrades the recording
	// (surfacing via rec.Err) but never interrupts the call.
	rec, err := inDialog.StartStereoRecording(wavFile)
	if err != nil {
		return err
	}
	// Must be closed for correct flushing. The caller keeps ownership of the
	// wav file: Close finalizes the WAV but never closes the fd.
	defer func() {
		if err := rec.Close(); err != nil {
			slog.Error("Failed to close recording", "error", err)
		}
	}()

	// Pump audio through the tap: read the inbound direction and write the
	// outbound one via the dialog handles, which now route through the tap.
	audioR, err := inDialog.AudioReader()
	if err != nil {
		return err
	}
	audioW, err := inDialog.AudioWriter()
	if err != nil {
		return err
	}
	_, err = media.Copy(audioR, audioW)
	if errors.Is(err, io.EOF) {
		// Call finished
		return nil
	}
	return err
}
