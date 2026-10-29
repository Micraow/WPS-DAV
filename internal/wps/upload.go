package wps

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Hashes carries the digests the WPS upload API requires before bytes move.
type Hashes struct {
	SHA1   string
	SHA256 string
	MD5    string
	Size   int64
}

// HashReader consumes r fully and returns its digests.
func HashReader(r io.Reader) (Hashes, error) {
	h1 := sha1.New()
	h256 := sha256.New()
	h5 := md5.New()
	n, err := io.Copy(io.MultiWriter(h1, h256, h5), r)
	if err != nil {
		return Hashes{}, err
	}
	return Hashes{
		SHA1:   hex.EncodeToString(h1.Sum(nil)),
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		MD5:    hex.EncodeToString(h5.Sum(nil)),
		Size:   n,
	}, nil
}

// UploadOptions describes one upload.
type UploadOptions struct {
	GroupID  int64
	ParentID int64
	Name     string
	Hashes   Hashes
	// OverwriteFileID, when non-zero, replaces an existing file version.
	OverwriteFileID int64
}

// Upload performs the three-stage WPS upload:
// create_update → PUT to the store URL → commit.
// body must provide exactly Hashes.Size bytes.
func (c *Client) Upload(ctx context.Context, opts UploadOptions, body io.Reader) (commitResp, error) {
	var out commitResp
	plan, err := c.createUpdate(ctx, opts)
	if err != nil {
		return out, err
	}

	stored, err := c.putStore(ctx, plan, opts.Hashes.Size, body)
	if err != nil {
		return out, err
	}

	commitBody := map[string]any{
		"etag":     stored.ETag,
		"groupid":  opts.GroupID,
		"key":      stored.Key,
		"name":     opts.Name,
		"parentid": opts.ParentID,
		"sha1":     stored.SHA1,
		"size":     opts.Hashes.Size,
		"store":    plan.Store,
		"storekey": stored.Key,
	}
	if opts.OverwriteFileID != 0 {
		commitBody["file_id"] = opts.OverwriteFileID
		commitBody["isUpNewVer"] = true
	}
	data, status, err := c.doJSON(ctx, "POST", c.driveURL("/api/v5/files/file"), commitBody)
	if err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("commit: decode: %w (http %d)", err, status)
	}
	if status != http.StatusOK || (out.Result != "" && out.Result != "ok") {
		return out, fmt.Errorf("commit: http %d result=%q msg=%q", status, out.Result, out.Msg)
	}
	return out, nil
}

func (c *Client) createUpdate(ctx context.Context, opts UploadOptions) (uploadPlan, error) {
	var plan uploadPlan
	body := map[string]any{
		"group_id":      fmt.Sprint(opts.GroupID),
		"name":          opts.Name,
		"parent_id":     fmt.Sprint(opts.ParentID),
		"sha1":          opts.Hashes.SHA1,
		"sha256":        opts.Hashes.SHA256,
		"md5":           opts.Hashes.MD5,
		"size":          fmt.Sprint(opts.Hashes.Size),
		"client_stores": "ks3",
		"with_rapid":    true,
	}
	if opts.OverwriteFileID != 0 {
		body["file_id"] = fmt.Sprint(opts.OverwriteFileID)
	}
	data, status, err := c.doJSON(ctx, "PUT", c.driveURL("/api/v5/files/upload/create_update"), body)
	if err != nil {
		return plan, fmt.Errorf("create_update: %w", err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, fmt.Errorf("create_update: decode: %w (http %d)", err, status)
	}
	if status != http.StatusOK || (plan.Result != "" && plan.Result != "ok") {
		return plan, fmt.Errorf("create_update: http %d result=%q msg=%q", status, plan.Result, plan.Msg)
	}
	if plan.URL == "" {
		return plan, fmt.Errorf("create_update: response has no upload url")
	}
	return plan, nil
}

type storeResult struct {
	Key  string
	ETag string
	SHA1 string
}

func (c *Client) putStore(ctx context.Context, plan uploadPlan, size int64, body io.Reader) (storeResult, error) {
	method := plan.Method
	if method == "" {
		method = http.MethodPut
	}

	var reqBody io.Reader = body
	var contentType string
	if len(plan.Request.FormData) > 0 {
		// Some stores expect a multipart form with the file in "file".
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			for k, v := range plan.Request.FormData {
				_ = mw.WriteField(k, v)
			}
			fw, err := mw.CreateFormFile("file", "blob")
			if err == nil {
				_, err = io.Copy(fw, body)
			}
			if err == nil {
				err = mw.Close()
			}
			pw.CloseWithError(err)
		}()
		reqBody = pr
		contentType = mw.FormDataContentType()
	}

	req, err := http.NewRequestWithContext(ctx, method, plan.URL, reqBody)
	if err != nil {
		return storeResult{}, err
	}
	for k, v := range plan.Request.Headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = size
	req.Header.Set("User-Agent", c.ua)

	// Reuse the shared no-timeout store client; context bounds the transfer.
	resp, err := c.store.Do(req)
	if err != nil {
		return storeResult{}, fmt.Errorf("store put: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	for _, code := range plan.Response.ExpectCode {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		return storeResult{}, fmt.Errorf("store put: http %d: %s", resp.StatusCode, snippet(respBody))
	}

	var pr uploadPutResp
	_ = json.Unmarshal(respBody, &pr)

	res := storeResult{ETag: respArg(plan.Response.ArgsETag, resp.Header, respBody)}
	if res.ETag == "" {
		res.ETag = normalizeETag(resp.Header.Get("ETag"))
	}
	if res.ETag == "" {
		res.ETag = normalizeETag(pr.MD5)
	}
	res.Key = respArg(plan.Response.ArgsKey, resp.Header, respBody)
	if res.Key == "" && pr.NewFilename != "" {
		res.Key = pr.NewFilename
	}
	if res.Key == "" && len(pr.Sha1) == 40 {
		res.Key = pr.Sha1
	}
	if res.Key == "" {
		res.Key = resp.Header.Get("x-obs-save-key")
	}
	res.SHA1 = pr.Sha1
	if res.SHA1 == "" && len(res.Key) == 40 {
		res.SHA1 = res.Key
	}
	if res.Key == "" {
		return storeResult{}, fmt.Errorf("store put: could not determine object key")
	}
	return res, nil
}

func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}
