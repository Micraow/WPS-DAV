// Package logging provides leveled logging for wpsdav: colored output on
// interactive terminals, plain text otherwise, and optional tee-to-file.
package logging

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"wpsdav/internal/ui"
)

const (
	levelInfo = iota
	levelWarn
	levelError
)

var (
	errLog  = log.New(os.Stderr, "wpsdav: ", log.LstdFlags|log.Lmsgprefix)
	fileLog *log.Logger
)

// SetFile tees log output to an append-only file (0600, directory created).
// An empty path is a no-op. Calling it again with the same or another path
// after a successful call is ignored; use it once at startup.
func SetFile(path string) error {
	if path == "" || fileLog != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fileLog = log.New(f, "wpsdav: ", log.LstdFlags|log.Lmsgprefix)
	return nil
}

// Infof logs an informational message.
func Infof(format string, args ...any) { emit(levelInfo, fmt.Sprintf(format, args...)) }

// Warnf logs a warning.
func Warnf(format string, args ...any) { emit(levelWarn, fmt.Sprintf(format, args...)) }

// Errorf logs an error.
func Errorf(format string, args ...any) { emit(levelError, fmt.Sprintf(format, args...)) }

// Fatalf logs an error and exits with status 1.
func Fatalf(format string, args ...any) {
	emit(levelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Transfer logs a transfer summary line.
func Transfer(kind, path string, size int64, d time.Duration) {
	emit(levelInfo, ui.TransferLine(kind, path, size, d))
}

func emit(level int, msg string) {
	errLog.Output(2, decorate(level, msg))
	if fileLog != nil {
		fileLog.Output(2, msg)
	}
}

func decorate(level int, msg string) string {
	if !ui.Enabled() {
		return msg
	}
	switch level {
	case levelWarn:
		return ui.Yellow("warning:") + " " + msg
	case levelError:
		return ui.Red("error:") + " " + msg
	default:
		return msg
	}
}
