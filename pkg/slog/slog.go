package slog

import (
	"log/slog"
)

// Error returns an Attr for an error.
func Error(err error) slog.Attr {
	return slog.String("error", err.Error())
}

// Uint16 converts an uint16 to an uint64 and returns
// an Attr with that value.
func Uint16(key string, v uint16) slog.Attr {
	return slog.Uint64(key, uint64(v))
}

func MapVerbosityToLogLevel(v int) slog.Level {
	switch {
	case v >= 4:
		return slog.LevelDebug
	case v >= 3:
		return slog.LevelInfo
	case v >= 2:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
