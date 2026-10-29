// Package auth provides credential sources (providers) for the WPS client.
//
// A provider owns one concern: producing a usable Cookie header for the WPS
// API, either from a manually maintained file or by logging in through an
// identity provider. The rest of the program only sees the Provider
// interface, so authentication strategies stay isolated from HTTP, storage
// and CLI code.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"wpsdav/internal/config"
)

// ErrManual reports that a provider has no automatic login path.
var ErrManual = errors.New("provider does not support automatic login")

// ErrNoCredentials reports that a login-capable provider has no credentials
// configured (cached cookies may still be usable).
var ErrNoCredentials = errors.New("provider has no credentials configured")

// Provider is a source of WPS cookie headers.
type Provider interface {
	// Name identifies the provider (for logs and diagnostics).
	Name() string

	// Renew obtains a fresh cookie header by logging in. It is only called
	// when the cached credentials are missing or rejected.
	Renew(ctx context.Context) (string, error)

	// Run keeps the credentials fresh and calls notify whenever a new cookie
	// becomes available. It blocks until ctx is done.
	Run(ctx context.Context, notify func(cookie string))
}

// Seedable is implemented by providers that can adopt a cached cookie header
// (lets a password-less process renew sessions that carry a refresh token).
type Seedable interface {
	Seed(cookie string) bool
}

// New builds the provider selected by the configuration.
func New(cfg *config.Config) (Provider, error) {
	switch cfg.Auth.Mode {
	case "", "cookie":
		return NewCookieProvider(cfg.CookieFile), nil
	case "buaa":
		user := strings.TrimSpace(cfg.Auth.SSO.Username)
		if user == "" {
			user = strings.TrimSpace(os.Getenv("WPSDAV_SSO_USERNAME"))
		}
		pass := cfg.Auth.SSO.Password
		if pass == "" {
			pass = os.Getenv("WPSDAV_SSO_PASSWORD")
		}
		entry := cfg.Auth.SSO.EntryURL
		interval := time.Duration(cfg.Auth.SSO.RefreshMinutes) * time.Minute
		// Credentials are optional: a cached cookie may still be valid, and
		// login can also be provided per command (`wpsdav login -provider buaa`).
		return NewBuaaProvider(user, pass, entry, interval), nil
	default:
		return nil, fmt.Errorf("unknown auth.mode %q", cfg.Auth.Mode)
	}
}
