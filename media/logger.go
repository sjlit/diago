package media

import "log/slog"

// DefaultLogger returns the fallback logger of the media package. It is used
// only by sessions without a MediaSession.Logger and by package-level entry
// points that carry no session (raw DTLS helpers, direct RTPPacketReader
// construction). Configure per-session loggers (MediaSession.Logger,
// diago.MediaConfig.Logger) instead of a process-wide one.
func DefaultLogger() *slog.Logger {
	return slog.Default()
}
