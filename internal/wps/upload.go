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
	"log"
	"math/rand"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Upload retries cover transient upstream failures only. Delays grow
// exponentially from upload_backoff, capped here so a dead host cannot stall
// a transfer indefinitely.
const maxUploadBackoff = 30 * time.Second

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

// Upload performs the three-stage WPS upload with retries:
// create_update → PUT to the store URL → commit.
// body must provide exactly Hashes.Size bytes and must be seekable so each
// store attempt can restart from offset zero.
func (c *Client) Upload(ctx context.Context, opts UploadOptions, body io.ReadSeeker) (commitResp, error) {
	var out commitResp
	var plan uploadPlan
	if err := c.retry(ctx, "create_update", func() error {
		var err error
		plan, err = c.createUpdate(ctx, opts)
		return err
	}); err != nil {
		return out, err
	}

	var stored storeResult
	if err := c.retry(ctx, "store put", func() error {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind upload body: %w", err)
		}
		var err error
		stored, err = c.putStore(ctx, plan, opts.Hashes.Size, body)
		return err
	}); err != nil {
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
	if err := c.retry(ctx, "commit", func() error {
		var err error
		out, err = c.commit(ctx, commitBody)
		return err
	}); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) commit(ctx context.Context, body map[string]any) (commitResp, error) {
	var out commitResp
	data, status, err := c.doJSON(ctx, "POST", c.driveURL("/api/v5/files/file"), body)
	if err != nil {
		return out, transportError("commit", "", err)
	}
	decodeErr := json.Unmarshal(data, &out)
	if status != http.StatusOK {
		ae := &APIError{Op: "commit", Status: status, Result: out.Result, Msg: out.Msg}
		if decodeErr != nil {
			ae.Body = snippet(data)
		}
		return out, ae
	}
	if decodeErr != nil {
		return out, &APIError{Op: "commit", Status: status, Body: snippet(data),
			Err: fmt.Errorf("decode: %w", decodeErr)}
	}
	if out.Result != "" && out.Result != "ok" {
		return out, &APIError{Op: "commit", Status: status, Result: out.Result, Msg: out.Msg}
	}
	return out, nil
}

// retry runs fn and repeats transient failures with exponential, jittered
// backoff. c.uploadRetries is the number of attempts after the first one.
func (c *Client) retry(ctx context.Context, op string, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if attempt >= c.uploadRetries || !retryable(err) {
			return err
		}
		delay := c.retryDelay(attempt, err)
		log.Printf("wps: %s retry %d/%d in %s: %v", op, attempt+1,
			c.uploadRetries, delay.Round(time.Millisecond), err)
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
	}
}

func (c *Client) retryDelay(attempt int, err error) time.Duration {
	base := c.uploadBackoff
	if base <= 0 {
		return 0
	}
	d := base
	for i := 0; i < attempt && d < maxUploadBackoff; i++ {
		d *= 2
	}
	if isRateLimited(err) {
		// WPS asks us to back off explicitly; doubling the wait costs little
		// and avoids burning a retry on the same throttle window.
		d *= 2
	}
	if d > maxUploadBackoff {
		d = maxUploadBackoff
	}
	// Equal jitter: half fixed, half random, so parallel transfers do not
	// retry in lockstep and hammer an already-struggling host.
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int63n(int64(half)+1))
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
		return plan, transportError("create_update", "", err)
	}
	decodeErr := json.Unmarshal(data, &plan)
	if status != http.StatusOK {
		ae := &APIError{Op: "create_update", Status: status,
			Result: plan.Result, Msg: plan.Msg}
		if decodeErr != nil {
			ae.Body = snippet(data)
		}
		return plan, ae
	}
	if decodeErr != nil {
		return plan, &APIError{Op: "create_update", Status: status, Body: snippet(data),
			Err: fmt.Errorf("decode: %w", decodeErr)}
	}
	if plan.Result != "" && plan.Result != "ok" {
		return plan, &APIError{Op: "create_update", Status: status,
			Result: plan.Result, Msg: plan.Msg}
	}
	if plan.URL == "" {
		return plan, &APIError{Op: "create_update", Err: fmt.Errorf("response has no upload url")}
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
	if rs, ok := reqBody.(io.ReadSeeker); ok {
		// Let net/http replay the body when it retries a request on a stale
		// HTTP/2 connection (server GOAWAY) instead of failing the upload.
		req.GetBody = func() (io.ReadCloser, error) {
			if _, err := rs.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			return io.NopCloser(rs), nil
		}
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
		return storeResult{}, transportError("store put", plan.URL, err)
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
		return storeResult{}, &APIError{Op: "store put", Status: resp.StatusCode,
			Body: snippet(respBody), RetryAfter: parseRetryAfter(resp.Header)}
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
		return storeResult{}, &APIError{Op: "store put", Status: resp.StatusCode,
			Msg: "could not determine object key", Body: snippet(respBody)}
	}
	return res, nil
}

func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}
