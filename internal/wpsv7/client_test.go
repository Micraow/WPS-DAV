package wpsv7

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	}, bytes.NewReader([]byte("hello")))
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

func TestUploadMultipart(t *testing.T) {
	var parts []string
	var partMD5 []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		part := r.URL.Query().Get("part_number")
		b, _ := io.ReadAll(r.Body)
		parts = append(parts, part+":"+string(b))
		partMD5 = append(partMD5, r.Header.Get("Content-MD5"))
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
	mux.HandleFunc("/v7/drives/d1/files/0/create_multipart_upload_task", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"upload_id":   "u1",
				"upload_addr": store.URL + "/mp",
				"part_size":   4,
				"part_count":  2,
			},
		})
	})
	mux.HandleFunc("/v7/multipart_upload_tasks/u1/commit", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"id": "v7-id", "name": "a.bin", "size": 7},
		})
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	tokens := agent.NewTokenSource("apik:test")
	tokens.TokenURL = api.URL + "/token"
	client := New(tokens)
	client.BaseURL = api.URL

	f, err := client.Upload(context.Background(), UploadRequest{
		DriveID: "d1", Name: "a.bin", Size: 7, SHA256: "aa",
		MultipartThreshold: 1,
	}, bytes.NewReader([]byte("abcdefg")))
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "v7-id" {
		t.Fatalf("file = %+v", f)
	}
	if len(parts) != 2 || parts[0] != "1:abcd" || parts[1] != "2:efg" {
		t.Fatalf("parts = %v", parts)
	}
	for i, got := range partMD5 {
		want := base64.StdEncoding.EncodeToString(md5Sum(strings.TrimPrefix(parts[i], strconv.Itoa(i+1)+":")))
		if got != want {
			t.Fatalf("part %d md5 = %q want %q", i+1, got, want)
		}
	}
}

func md5Sum(s string) []byte {
	h := md5.Sum([]byte(s))
	return h[:]
}
