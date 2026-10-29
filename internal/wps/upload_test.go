package wps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wpsdav/internal/config"
)

func newTestClient(endpoint string) *Client {
	cfg := config.Default()
	c := New(cfg)
	c.endpoint = endpoint
	c.uploadRetries = cfg.UploadRetries
	c.uploadBackoff = 5 * time.Millisecond
	return c
}

func TestUploadRetriesTransientFailures(t *testing.T) {
	var createAttempts, storeAttempts, commitAttempts atomic.Int32
	key := strings.Repeat("a", 40)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v5/files/upload/create_update":
			if createAttempts.Add(1) <= 2 {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"result":"ok","method":"PUT","url":"` + "http://" + r.Host +
				`/store","store":"ks3","request":{"headers":{}},` +
				`"response":{"args_etag":"header.ETag","args_key":"header.x-obs-save-key"}}`))
		case "/store":
			if storeAttempts.Add(1) <= 2 {
				// Simulate the WPS storage CDN dropping the connection.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("response writer does not support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("x-obs-save-key", key)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case "/api/v5/files/file":
			if commitAttempts.Add(1) <= 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"result":"apiRateLimitExceede","msg":"您使用太频繁了，请稍等"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"result":"ok","id":123,"fsize":7}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	body := strings.NewReader("payload")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := c.Upload(ctx, UploadOptions{
		GroupID:  1,
		ParentID: 2,
		Name:     "a.txt",
		Hashes:   Hashes{Size: 7},
	}, body)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if out.ID != 123 {
		t.Fatalf("commit id = %d, want 123", out.ID)
	}
	if got := createAttempts.Load(); got != 3 {
		t.Errorf("create_update attempts = %d, want 3", got)
	}
	if got := storeAttempts.Load(); got != 3 {
		t.Errorf("store attempts = %d, want 3", got)
	}
	if got := commitAttempts.Load(); got != 2 {
		t.Errorf("commit attempts = %d, want 2", got)
	}
}

func TestUploadDoesNotRetryPermanentErrors(t *testing.T) {
	var createAttempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		createAttempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"result":"illegalFname","msg":"名字不能包含\\/:*?\"<>|"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Upload(context.Background(), UploadOptions{
		Name:   "bad:name.txt",
		Hashes: Hashes{Size: 7},
	}, strings.NewReader("payload"))
	if err == nil {
		t.Fatal("Upload succeeded, want error")
	}
	if got := createAttempts.Load(); got != 1 {
		t.Errorf("create_update attempts = %d, want 1 (no retry)", got)
	}
	status, _, ok := HTTPStatus(err)
	if !ok || status != http.StatusForbidden {
		t.Errorf("HTTPStatus = %d/%v, want 403/true", status, ok)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", transportError("store put", "https://x/y", errors.New("unexpected EOF")), true},
		{"429", &APIError{Status: 429}, true},
		{"503", &APIError{Status: 503}, true},
		{"duplicated", &APIError{Status: 403, Result: "fileTaskDuplicated"}, true},
		{"forbidden", &APIError{Status: 403, Result: "illegalFname"}, false},
		{"not found", os.ErrNotExist, false},
		{"exists", os.ErrExist, false},
		{"canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		ok      bool
		retryAt bool
	}{
		{"rate limit", &APIError{Status: 429}, http.StatusTooManyRequests, true, true},
		{"upstream 5xx", &APIError{Status: 503}, http.StatusBadGateway, true, false},
		{"transport", transportError("store put", "https://x/y", errors.New("EOF")), http.StatusBadGateway, true, false},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, true, false},
		{"illegal name", &APIError{Status: 403, Result: "illegalFname"}, http.StatusForbidden, true, false},
		{"stale cookie", &APIError{Status: 403, Result: "notLogin"}, http.StatusBadGateway, true, false},
		{"missing", os.ErrNotExist, 0, false, false},
		{"exists", os.ErrExist, 0, false, false},
	}
	for _, tc := range cases {
		status, retryAfter, ok := HTTPStatus(tc.err)
		if status != tc.status || ok != tc.ok {
			t.Errorf("%s: HTTPStatus = %d/%v, want %d/%v", tc.name, status, ok, tc.status, tc.ok)
		}
		if (retryAfter > 0) != tc.retryAt {
			t.Errorf("%s: retryAfter = %s, want present=%v", tc.name, retryAfter, tc.retryAt)
		}
	}
}

func TestRetryDelayBudget(t *testing.T) {
	c := New(config.Default())
	c.uploadBackoff = 2 * time.Second
	prev := time.Duration(0)
	for attempt := 0; attempt < 5; attempt++ {
		d := c.retryDelay(attempt, &APIError{Status: 503})
		if d <= 0 {
			t.Fatalf("attempt %d: zero delay", attempt)
		}
		if d > maxUploadBackoff {
			t.Fatalf("attempt %d: delay %s exceeds cap %s", attempt, d, maxUploadBackoff)
		}
		if d <= prev/2 {
			t.Fatalf("attempt %d: delay %s did not grow from %s", attempt, d, prev)
		}
		prev = d
	}
	normal := c.retryDelay(0, &APIError{Status: 503})
	limited := c.retryDelay(0, &APIError{Status: 429})
	if limited < normal {
		t.Fatalf("rate-limit delay = %s, want >= normal delay %s", limited, normal)
	}
}
