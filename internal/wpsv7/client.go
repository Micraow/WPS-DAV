// Package wpsv7 implements the subset of the official WPS 365 v7 API used
// for uploads: single PUT, multipart PUT, and commit.
package wpsv7

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	Commit  *http.Client // multipart merge may take minutes
}

// New builds a client for the given token source.
func New(tokens *agent.TokenSource) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Tokens:  tokens,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Store:   &http.Client{},
		Commit:  &http.Client{Timeout: 10 * time.Minute},
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
	// MultipartThreshold enables multipart uploads at or above this size.
	// Zero keeps the single-PUT path.
	MultipartThreshold int64
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

// Upload runs the single-PUT or multipart flow depending on size.
func (c *Client) Upload(ctx context.Context, req UploadRequest, body io.ReadSeeker) (File, error) {
	if req.DriveID == "" || req.Name == "" {
		return File{}, fmt.Errorf("v7 upload: drive and name are required")
	}
	if req.Size < 0 {
		return File{}, fmt.Errorf("v7 upload: invalid size")
	}
	if req.MultipartThreshold > 0 && req.Size >= req.MultipartThreshold {
		return c.uploadMultipart(ctx, req, body)
	}
	return c.uploadSingle(ctx, req, body)
}

func (c *Client) uploadSingle(ctx context.Context, req UploadRequest, body io.ReadSeeker) (File, error) {
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return File{}, err
	}
	plan, err := c.requestUpload(ctx, req)
	if err != nil {
		return File{}, err
	}
	if err := c.putStore(ctx, plan.store, req.Size, body); err != nil {
		return File{}, err
	}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data File   `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/v7/drives/%s/files/0/commit_upload", c.BaseURL, url.PathEscape(req.DriveID)),
		map[string]any{"upload_id": plan.uploadID}, &data); err != nil {
		return File{}, err
	}
	if data.Code != 0 {
		return File{}, fmt.Errorf("v7 commit_upload: code=%d msg=%s", data.Code, data.Msg)
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
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UploadID     string       `json:"upload_id"`
			StoreRequest storeRequest `json:"store_request"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/v7/drives/%s/files/0/request_upload", c.BaseURL, url.PathEscape(req.DriveID)),
		c.uploadBody(req), &data); err != nil {
		return plan, err
	}
	if data.Code != 0 {
		return plan, fmt.Errorf("v7 request_upload: code=%d msg=%s", data.Code, data.Msg)
	}
	if data.Data.UploadID == "" || data.Data.StoreRequest.URL == "" {
		return plan, fmt.Errorf("v7 request_upload: incomplete response")
	}
	plan.uploadID = data.Data.UploadID
	plan.store = data.Data.StoreRequest
	if plan.store.Method == "" {
		plan.store.Method = http.MethodPut
	}
	return plan, nil
}

func (c *Client) uploadBody(req UploadRequest) map[string]any {
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
	return body
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

// ---- multipart ----

type multipartTask struct {
	uploadID   string
	uploadAddr string
	partSize   int64
	partCount  int
}

func (c *Client) uploadMultipart(ctx context.Context, req UploadRequest, body io.ReadSeeker) (File, error) {
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return File{}, err
	}
	task, err := c.createMultipart(ctx, req)
	if err != nil {
		return File{}, err
	}
	fail := func(err error) (File, error) {
		c.abortMultipart(ctx, task.uploadID)
		return File{}, err
	}
	var offset int64
	for part := 1; part <= task.partCount; part++ {
		n := task.partSize
		if remaining := req.Size - offset; remaining < n {
			n = remaining
		}
		if n <= 0 {
			return fail(fmt.Errorf("v7 multipart: invalid part %d size", part))
		}
		if _, err := body.Seek(offset, io.SeekStart); err != nil {
			return fail(err)
		}
		sum := md5.New()
		if _, err := io.CopyN(sum, body, n); err != nil {
			return fail(fmt.Errorf("v7 multipart: read part %d: %w", part, err))
		}
		var perr error
		for attempt := 0; attempt < 3; attempt++ {
			if _, err := body.Seek(offset, io.SeekStart); err != nil {
				return fail(err)
			}
			perr = c.putPart(ctx, task.uploadAddr, part, n, sum.Sum(nil), body)
			if perr == nil {
				break
			}
			if attempt < 2 && !sleep(ctx, attempt) {
				return fail(ctx.Err())
			}
		}
		if perr != nil {
			return fail(fmt.Errorf("v7 multipart: part %d: %w", part, perr))
		}
		offset += n
	}
	if offset != req.Size {
		return fail(fmt.Errorf("v7 multipart: read %d of %d bytes", offset, req.Size))
	}
	return c.commitMultipart(ctx, task.uploadID)
}

func (c *Client) createMultipart(ctx context.Context, req UploadRequest) (multipartTask, error) {
	var task multipartTask
	body := c.uploadBody(req)
	body["mode"] = "sequential"
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UploadID   string `json:"upload_id"`
			UploadAddr string `json:"upload_addr"`
			PartSize   int64  `json:"part_size"`
			PartCount  int    `json:"part_count"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/v7/drives/%s/files/0/create_multipart_upload_task", c.BaseURL, url.PathEscape(req.DriveID)),
		body, &data); err != nil {
		return task, err
	}
	if data.Code != 0 {
		return task, fmt.Errorf("v7 create_multipart_upload_task: code=%d msg=%s", data.Code, data.Msg)
	}
	if data.Data.UploadID == "" || data.Data.UploadAddr == "" || data.Data.PartSize <= 0 || data.Data.PartCount <= 0 {
		return task, fmt.Errorf("v7 create_multipart_upload_task: incomplete response")
	}
	task.uploadID = data.Data.UploadID
	task.uploadAddr = data.Data.UploadAddr
	task.partSize = data.Data.PartSize
	task.partCount = data.Data.PartCount
	return task, nil
}

func (c *Client) putPart(ctx context.Context, addr string, part int, size int64, md5sum []byte, body io.Reader) error {
	u, err := url.Parse(addr)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("part_number", strconv.Itoa(part))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.LimitReader(body, size))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(md5sum))
	if err := c.auth(ctx, req); err != nil {
		return err
	}
	resp, err := c.Store.Do(req)
	if err != nil {
		return fmt.Errorf("v7 part %d: %w", part, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Op: fmt.Sprintf("part %d", part), Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return nil
}

func (c *Client) commitMultipart(ctx context.Context, uploadID string) (File, error) {
	endpoint := fmt.Sprintf("%s/v7/multipart_upload_tasks/%s/commit", c.BaseURL, url.PathEscape(uploadID))
	var last error
	for attempt := 0; attempt < 150; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
		if err != nil {
			return File{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.auth(ctx, req); err != nil {
			return File{}, err
		}
		resp, err := c.Commit.Do(req)
		if err != nil {
			last = fmt.Errorf("v7 commit_multipart: %w", err)
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var out struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
				Data File   `json:"data"`
			}
			_ = json.Unmarshal(data, &out)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && out.Code == 0 {
				return out.Data, nil
			}
			last = &APIError{Op: "commit_multipart", Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
			if out.Code == 409007001 || resp.StatusCode == http.StatusConflict {
				// Merge in progress: the documented backoff is at least 2s.
				if !sleepFor(ctx, 2*time.Second) {
					return File{}, ctx.Err()
				}
				continue
			}
		}
		if attempt < 2 && sleep(ctx, attempt) {
			continue
		}
		return File{}, last
	}
	return File{}, last
}

func (c *Client) abortMultipart(ctx context.Context, uploadID string) {
	endpoint := fmt.Sprintf("%s/v7/multipart_upload_tasks/%s/abort", c.BaseURL, url.PathEscape(uploadID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.auth(ctx, req); err != nil {
		return
	}
	resp, err := c.HTTP.Do(req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		resp.Body.Close()
	}
}

// ---- shared helpers ----

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
	return sleepFor(ctx, d)
}

func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
