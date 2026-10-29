package auth

import (
	"context"
	"log"
	"sync"
	"time"

	"wpsdav/internal/sso"
)

// BuaaProvider logs in through BUAA unified authentication and keeps the
// session fresh (grant_token refresh, full re-login as fallback).
type BuaaProvider struct {
	mgr     *sso.Manager
	hasCred bool
	refresh time.Duration

	mu      sync.Mutex
	current string
}

// NewBuaaProvider builds the provider. refresh <= 0 falls back to 240 minutes.
// Credentials may be empty: cached cookies still work, only renewal is disabled.
func NewBuaaProvider(username, password, entryURL string, refresh time.Duration) *BuaaProvider {
	if refresh <= 0 {
		refresh = 240 * time.Minute
	}
	return &BuaaProvider{
		mgr:     sso.NewManager(username, password, entryURL),
		hasCred: username != "" && password != "",
		refresh: refresh,
	}
}

// Name implements Provider.
func (p *BuaaProvider) Name() string { return "buaa" }

// Seed adopts a cookie obtained earlier (e.g. from the cookie cache file).
func (p *BuaaProvider) Seed(cookie string) bool {
	if !p.mgr.Seed(cookie) {
		return false
	}
	p.setCurrent(cookie)
	return true
}

// Renew obtains fresh credentials: grant_token first (works with rtk alone),
// then a full login if credentials are configured.
func (p *BuaaProvider) Renew(ctx context.Context) (string, error) {
	if p.mgr.Session() != nil {
		if sess, err := p.mgr.Refresh(ctx); err == nil {
			p.setCurrent(sess.CookieHeader())
			log.Printf("SSO session refreshed (expires_in=%ds)", sess.ExpiresIn)
			return sess.CookieHeader(), nil
		}
	}
	if !p.hasCred {
		return "", ErrNoCredentials
	}
	sess, err := p.mgr.Login(ctx)
	if err != nil {
		return "", err
	}
	p.setCurrent(sess.CookieHeader())
	log.Printf("SSO login ok: %s (uid=%d company=%d)", sess.Nickname, sess.UID, sess.CompanyID)
	return sess.CookieHeader(), nil
}

// Run implements Provider: refresh on an interval, re-login on failure.
func (p *BuaaProvider) Run(ctx context.Context, notify func(string)) {
	if !p.hasCred && p.mgr.Session() == nil {
		log.Printf("buaa provider has neither credentials nor a cached session; background refresh disabled")
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(p.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		sess, err := p.mgr.Refresh(rctx)
		cancel()
		if err != nil {
			log.Printf("SSO refresh failed: %v", err)
			continue
		}
		p.setCurrent(sess.CookieHeader())
		log.Printf("SSO session refreshed (expires_in=%ds)", sess.ExpiresIn)
		notify(sess.CookieHeader())
	}
}

func (p *BuaaProvider) setCurrent(cookie string) {
	p.mu.Lock()
	p.current = cookie
	p.mu.Unlock()
}

// Current returns the cookie from the last successful login/refresh.
func (p *BuaaProvider) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}
