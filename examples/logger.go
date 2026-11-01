// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

// Package examples contains runnable diago examples. Subdirectories are
// separate main packages; this file holds shared logging setup.
package examples

import (
	"log/slog"
	"os"

	"github.com/emiago/sipgo/sip"
)

// SetupLogger configures process-wide generic debug switches for the
// examples. Per-dialog media tracing is configured with
// diago.MediaConfig{Debug: true} (or MediaConfig.Tracer) instead of a global.
func SetupLogger() {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		lvl = slog.LevelInfo
	}
	slog.SetLogLoggerLevel(lvl)
	sip.SIPDebug = os.Getenv("SIP_DEBUG") == "true"
	sip.TransactionFSMDebug = os.Getenv("SIP_TRANSACTION_DEBUG") == "true"
}
