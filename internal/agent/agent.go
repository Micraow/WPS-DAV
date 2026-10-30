// Package agent exchanges a WPS AI Agent API key for short-lived access
// tokens. The key format is "apik:<id>.<secret>"; tokens are JWTs valid for
// 12 hours.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultTokenURL is the WPS agent token endpoint.
const DefaultTokenURL = "https://account.wps.cn/api/authorization/agent/v1/token"

// ErrMissingKey is returned when no API key is configured.
var ErrMissingKey = errors.New("agent API key is empty")

// TokenSource caches an access token and renews it before expiry.
type TokenSource struct {
	Key      string
	TokenURL string
	HTTP     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewTokenSource builds a token source. An empty key is allowed; Token will
// return ErrMissingKey.
func NewTokenSource(key string) *TokenSource {
	return &TokenSource{
		Key:      strings.TrimSpace(key),
		TokenURL: DefaultTokenURL,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Token returns a valid access token, exchanging the API key if needed.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	if t.Key == "" {
		return "", ErrMissingKey
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Add(5*time.Minute).Before(t.expires) {
		return t.token, nil
	}
	token, expiresIn, err := t.exchange(ctx)
	if err != nil {
		return "", err
	}
	t.token = token
	t.expires = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return token, nil
}

// Invalidate drops the cached token (e.g. after a 401).
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	t.token = ""
	t.expires = time.Time{}
	t.mu.Unlock()
}

func (t *TokenSource) exchange(ctx context.Context) (string, int, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type": "api_key",
		"api_key":    t.Key,
	})
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, bytes.NewReader(body))
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "wpsdav")
		resp, err := t.HTTP.Do(req)
		if err != nil {
			last = err
			if !sleep(ctx, attempt) {
				return "", 0, ctx.Err()
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			if !sleep(ctx, attempt) {
				return "", 0, ctx.Err()
			}
			continue
		}
		var result struct {
			Result string `json:"result"`
			Msg    string `json:"msg"`
			Data   struct {
				AccessToken string `json:"access_token"`
				ExpiresIn   int    `json:"expires_in"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			last = fmt.Errorf("agent token: invalid response (http %d)", resp.StatusCode)
			if resp.StatusCode >= 500 && sleep(ctx, attempt) {
				continue
			}
			return "", 0, last
		}
		if resp.StatusCode != http.StatusOK || result.Result != "ok" || result.Data.AccessToken == "" {
			return "", 0, fmt.Errorf("agent token: http %d result=%q msg=%q", resp.StatusCode, result.Result, result.Msg)
		}
		if result.Data.ExpiresIn <= 0 {
			result.Data.ExpiresIn = 43200 // documented default: 12h
		}
		return result.Data.AccessToken, result.Data.ExpiresIn, nil
	}
	if last == nil {
		last = errors.New("agent token: request failed")
	}
	return "", 0, last
}

func sleep(ctx context.Context, attempt int) bool {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
