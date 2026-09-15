package media

import (
	"log/slog"
	"sync/atomic"
)

var defLoggerPtr atomic.Pointer[slog.Logger]

// SetDefaultLogger sets the process-wide fallback logger of the media
// package. It is used only by sessions without a MediaSession.Logger and by
// package-level entry points that carry no session (raw DTLS helpers, direct
// RTPPacketReader construction).
//
// Deprecated: configure diago.MediaConfig.Logger (or set
// MediaSession.Logger) so each session carries its own logger. This global
// remains for direct media package users and tests.
func SetDefaultLogger(l *slog.Logger) {
	defLoggerPtr.Store(l)
}

func DefaultLogger() *slog.Logger {
	if l := defLoggerPtr.Load(); l != nil {
		return l
	}
	return slog.Default()
}
