package wpsv7

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wpsdav/internal/agent"
)

func TestUploadFlowClean(t *testing.T) {
	var gotBody string
	var gotAuth string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer store.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": "ok",
			"data":   map[string]any{"access_token": "tok", "expires_in": 43200},
		})
	})
	mux.HandleFunc("/v7/drives/d1/files/0/request_upload", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["on_name_conflict"] != "overwrite" || body["parent_path"] == nil {
			t.Errorf("unexpected body: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"upload_id": "u1",
				"store_request": map[string]any{
					"method": "PUT",
					"url":    store.URL + "/put",
				},
			},
		})
	})
	mux.HandleFunc("/v7/drives/d1/files/0/commit_upload", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"id": "v7-id", "name": "a.txt", "size": 5},
		})
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	tokens := agent.NewTokenSource("apik:test")
	tokens.TokenURL = api.URL + "/token"
	client := New(tokens)
	client.BaseURL = api.URL

	f, err := client.Upload(context.Background(), UploadRequest{
		DriveID: "d1", ParentPath: []string{"docs"}, Name: "a.txt",
		Size: 5, SHA256: "aa", MD5: "bb", Overwrite: true,
	}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "v7-id" || f.Name != "a.txt" {
		t.Fatalf("file = %+v", f)
	}
	if gotBody != "hello" || gotAuth != "Bearer tok" {
		t.Fatalf("store body=%q auth=%q", gotBody, gotAuth)
	}
}
