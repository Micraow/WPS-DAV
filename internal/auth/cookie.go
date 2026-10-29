package auth

import (
	"context"
	"os"
	"strings"
	"time"

	"wpsdav/internal/logging"
)

// CookieProvider serves a cookie header maintained by hand (browser export).
// It watches the file and pushes changes, so a running server picks up a
// refreshed cookie without a restart.
type CookieProvider struct {
	path string
	poll time.Duration
}

// NewCookieProvider reads the cookie header from path.
func NewCookieProvider(path string) *CookieProvider {
	return &CookieProvider{path: path, poll: 5 * time.Second}
}

// Name implements Provider.
func (p *CookieProvider) Name() string { return "cookie" }

// AutoRenew implements Provider: manual cookies cannot be re-issued.
func (p *CookieProvider) AutoRenew() bool { return false }

// Renew implements Provider: manual cookies cannot be re-issued.
func (p *CookieProvider) Renew(context.Context) (string, error) { return "", ErrManual }

// Run implements Provider by polling the cookie file.
func (p *CookieProvider) Run(ctx context.Context, notify func(string)) {
	var lastMod time.Time
	var lastSize int64 = -1
	load := func() {
		fi, err := os.Stat(p.path)
		if err != nil {
			return
		}
		if fi.ModTime().Equal(lastMod) && fi.Size() == lastSize {
			return
		}
		data, err := os.ReadFile(p.path)
		if err != nil {
			return
		}
		cookie := strings.TrimSpace(string(data))
		if cookie == "" {
			return
		}
		lastMod, lastSize = fi.ModTime(), fi.Size()
		logging.Infof("cookie reloaded from %s (%d bytes)", p.path, len(cookie))
		notify(cookie)
	}
	load()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			load()
		}
	}
}
