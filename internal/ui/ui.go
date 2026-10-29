// Package ui provides terminal-aware colors and human-readable transfer stats.
// Colors are disabled automatically when output is not a TTY, when NO_COLOR
// is set, or when TERM=dumb.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

var enabled = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd())) || term.IsTerminal(int(os.Stderr.Fd()))
}()

// Enabled reports whether colored output is active.
func Enabled() bool { return enabled }

func wrap(code, s string) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Bold, Dim, Green, Red, Yellow, Cyan style text when colors are enabled.
func Bold(s string) string   { return wrap("1", s) }
func Dim(s string) string    { return wrap("2", s) }
func Green(s string) string  { return wrap("32", s) }
func Red(s string) string    { return wrap("31", s) }
func Yellow(s string) string { return wrap("33", s) }
func Cyan(s string) string   { return wrap("36", s) }

// Tag renders a colored transfer tag such as "upload" or "download".
func Tag(kind string) string {
	switch kind {
	case "upload":
		return Green("[" + kind + "]")
	case "download":
		return Cyan("[" + kind + "]")
	default:
		return Bold("[" + kind + "]")
	}
}

// Bytes formats a byte count with binary units.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Rate formats bytes per second.
func Rate(n int64, d time.Duration) string {
	if d <= 0 {
		return "?"
	}
	return Bytes(int64(float64(n)/d.Seconds())) + "/s"
}

// Duration formats a transfer duration with a sensible precision.
func Duration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// TransferLine builds a human-readable transfer summary.
func TransferLine(kind, path string, size int64, d time.Duration) string {
	return fmt.Sprintf("%s %s  %s in %s (%s)",
		Tag(kind), path, Bytes(size), Duration(d), Rate(size, d))
}
