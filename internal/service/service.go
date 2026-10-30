// Package service manages the systemd user unit used for autostart.
package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// UnitName is the installed systemd user unit.
const UnitName = "wpsdav.service"

// ErrUnsupported means this platform has no systemd user manager.
var ErrUnsupported = errors.New("systemd user services are not available on this platform")

// Status describes the unit's state.
type Status struct {
	Supported   bool
	UnitPresent bool
	Enabled     bool
	Active      bool
}

// Available reports whether a systemd user manager is reachable.
func Available() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

// Get returns the current unit status.
func Get(ctx context.Context) (Status, error) {
	if !Available() {
		return Status{}, ErrUnsupported
	}
	s := Status{Supported: true}
	if exec.CommandContext(ctx, "systemctl", "--user", "cat", UnitName).Run() == nil {
		s.UnitPresent = true
	}
	s.Enabled = stateIs(ctx, "is-enabled", "enabled")
	s.Active = stateIs(ctx, "is-active", "active")
	return s, nil
}

// Enable enables and starts the user unit.
func Enable(ctx context.Context) error {
	if !Available() {
		return ErrUnsupported
	}
	if err := run(ctx, "daemon-reload"); err != nil {
		return err
	}
	if exec.CommandContext(ctx, "systemctl", "--user", "cat", UnitName).Run() != nil {
		return fmt.Errorf("%s not found; install wpsdav-bin (it ships the unit) or add a user unit", UnitName)
	}
	return run(ctx, "enable", "--now", UnitName)
}

// Disable stops the unit and removes it from autostart.
func Disable(ctx context.Context) error {
	if !Available() {
		return ErrUnsupported
	}
	return run(ctx, "disable", "--now", UnitName)
}

// Restart restarts the unit if it is running.
func Restart(ctx context.Context) error {
	if !Available() {
		return ErrUnsupported
	}
	return run(ctx, "restart", UnitName)
}

func stateIs(ctx context.Context, verb, want string) bool {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", verb, UnitName).Output()
	return err == nil && strings.TrimSpace(string(out)) == want
}

func run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}
