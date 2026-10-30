// Package wpsv7 implements the small subset of the official WPS 365 v7 API
// needed to upload files (request_upload → PUT → commit_upload).
package wpsv7

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wpsdav/internal/agent"
)

// DefaultBaseURL is the public WPS 365 API gateway.
const DefaultBaseURL = "https://openapi.wps.cn"

// Client talks to the v7 API with an agent access token.
type Client struct {
	BaseURL string
	Tokens  *agent.TokenSource
	HTTP    *http.Client // API calls
	Store   *http.Client // byte transfers: no overall timeout
}

// New builds a client for the given token source.
func New(tokens *agent.TokenSource) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Tokens:  tokens,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Store:   &http.Client{},
	}
}

// UploadRequest describes one file to store through the v7 API.
type UploadRequest struct {
	DriveID    string
	ParentPath []string // path components relative to the drive root
	Name       string
	Size       int64
	SHA256     string
	MD5        string
	Overwrite  bool
}

// File is the file metadata returned by commit_upload.
type File struct {
	ID       string `json:"id"`
	DriveID  string `json:"drive_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Mtime    int64  `json:"mtime"`
}

// APIError is a non-2xx response from the v7 gateway.
type APIError struct {
	Op     string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("v7 %s: http %d: %s", e.Op, e.Status, e.Body)
}

// Upload runs request_upload → PUT → commit_upload.
func (c *Client) Upload(ctx context.Context, req UploadRequest, body io.Reader) (File, error) {
	var out File
	if req.DriveID == "" || req.Name == "" {
		return out, fmt.Errorf("v7 upload: drive and name are required")
	}
	if req.Size < 0 {
		return out, fmt.Errorf("v7 upload: invalid size")
	}

	plan, err := c.requestUpload(ctx, req)
	if err != nil {
		return out, err
	}

	if err := c.putStore(ctx, plan.store, req.Size, body); err != nil {
		return out, err
	}

	commitBody := map[string]any{"upload_id": plan.uploadID}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data File   `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/v7/drives/%s/files/0/commit_upload", c.BaseURL, url.PathEscape(req.DriveID)),
		commitBody, &data); err != nil {
		return out, err
	}
	if data.Code != 0 {
		return out, fmt.Errorf("v7 commit_upload: code=%d msg=%s", data.Code, data.Msg)
	}
	return data.Data, nil
}

type uploadPlan struct {
	uploadID string
	store    storeRequest
}

type storeRequest struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}

func (c *Client) requestUpload(ctx context.Context, req UploadRequest) (uploadPlan, error) {
	var plan uploadPlan
	hashes := make([]map[string]string, 0, 2)
	if req.SHA256 != "" {
		hashes = append(hashes, map[string]string{"sum": req.SHA256, "type": "sha256"})
	}
	if req.MD5 != "" {
		hashes = append(hashes, map[string]string{"sum": req.MD5, "type": "md5"})
	}
	conflict := "rename"
	if req.Overwrite {
		conflict = "overwrite"
	}
	body := map[string]any{
		"name":             req.Name,
		"size":             req.Size,
		"hashes":           hashes,
		"on_name_conflict": conflict,
	}
	if len(req.ParentPath) > 0 {
		body["parent_path"] = req.ParentPath
	}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UploadID     string `json:"upload_id"`
			StoreRequest struct {
				Method  string `json:"method"`
				URL     string `json:"url"`
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"store_request"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/v7/drives/%s/files/0/request_upload", c.BaseURL, url.PathEscape(req.DriveID)),
		body, &data); err != nil {
		return plan, err
	}
	if data.Code != 0 {
		return plan, fmt.Errorf("v7 request_upload: code=%d msg=%s", data.Code, data.Msg)
	}
	if data.Data.UploadID == "" || data.Data.StoreRequest.URL == "" {
		return plan, fmt.Errorf("v7 request_upload: incomplete response")
	}
	plan.uploadID = data.Data.UploadID
	plan.store.Method = data.Data.StoreRequest.Method
	if plan.store.Method == "" {
		plan.store.Method = http.MethodPut
	}
	plan.store.URL = data.Data.StoreRequest.URL
	plan.store.Headers = data.Data.StoreRequest.Headers
	return plan, nil
}

func (c *Client) putStore(ctx context.Context, store storeRequest, size int64, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, store.Method, store.URL, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	for _, h := range store.Headers {
		req.Header.Set(h.Name, h.Value)
	}
	if err := c.auth(ctx, req); err != nil {
		return err
	}
	resp, err := c.Store.Do(req)
	if err != nil {
		return fmt.Errorf("v7 store put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Op: "store put", Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := c.auth(ctx, req); err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt < 2 && sleep(ctx, attempt) {
				continue
			}
			return fmt.Errorf("v7 request: %w", err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return fmt.Errorf("v7 response: %w", rerr)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.Tokens.Invalidate()
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			if attempt < 2 {
				if !sleep(ctx, attempt) {
					return ctx.Err()
				}
				continue
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &APIError{Op: method + " " + redact(endpoint), Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("v7 decode: %w", err)
			}
		}
		return nil
	}
	return fmt.Errorf("v7 request: unauthorized after token refresh")
}

func (c *Client) auth(ctx context.Context, req *http.Request) error {
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func redact(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.RawQuery != "" {
		parsed.RawQuery = "…"
		return parsed.String()
	}
	return u
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
