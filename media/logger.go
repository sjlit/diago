package media

import (
	"log/slog"
	"sync/atomic"
)

var defLoggerPtr atomic.Pointer[slog.Logger]

// SetDefaultLogger sets default logger that will be used withing sip package
// Must be called before any usage of library
func SetDefaultLogger(l *slog.Logger) {
	defLoggerPtr.Store(l)
}

func DefaultLogger() *slog.Logger {
	if l := defLoggerPtr.Load(); l != nil {
		return l
	}
	return slog.Default()
}
