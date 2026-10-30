package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenSourceCachesAndRefreshes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "api_key" || body["api_key"] != "apik:test" {
			t.Errorf("unexpected body: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": "ok",
			"data":   map[string]any{"access_token": "tok", "expires_in": 43200},
		})
	}))
	defer srv.Close()

	ts := NewTokenSource("apik:test")
	ts.TokenURL = srv.URL
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := ts.Token(ctx)
		if err != nil || tok != "tok" {
			t.Fatalf("token: %q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Fatalf("expected one exchange, got %d", calls)
	}
	ts.Invalidate()
	if _, err := ts.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected refresh after invalidate, got %d", calls)
	}
}

func TestTokenSourceMissingKey(t *testing.T) {
	ts := NewTokenSource("")
	if _, err := ts.Token(context.Background()); err != ErrMissingKey {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}
