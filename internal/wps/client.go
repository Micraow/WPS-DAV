package wps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"wpsdav/internal/config"
)

const (
	endpointPersonal = "https://drive.kdocs.cn"
	endpointBusiness = "https://365.kdocs.cn"
	prefixBusiness   = "/3rd/drive"
	loginCheckURL    = "https://account.kdocs.cn/api/v3/islogin"
	defaultUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// Client talks to the WPS netdisk API with a browser cookie.
type Client struct {
	http    *http.Client
	store   *http.Client // byte transfers: no overall timeout, reused conns
	mode    config.Mode
	ua      string
	cookie  atomic.Value // string
	company atomic.Int64 // discovered company id (business)
	IsLogin LoginState

	uploadRetries int
	uploadBackoff time.Duration
	endpoint      string // overrides the drive host in tests
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
}

// New builds a client. cookie may be empty; use SetCookie to install one.
func New(cfg *config.Config) *Client {
	c := &Client{
		http: &http.Client{
			Timeout:   60 * time.Second,
			Transport: newTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		store: &http.Client{
			Timeout:   0, // uploads may take arbitrarily long
			Transport: newTransport(),
		},
		mode:          cfg.Mode,
		ua:            cfg.CustomUA,
		uploadRetries: cfg.UploadRetries,
		uploadBackoff: time.Duration(cfg.UploadRetryBackoffSeconds * float64(time.Second)),
	}
	if c.ua == "" {
		c.ua = defaultUA
	}
	c.cookie.Store("")
	if cfg.CompanyID > 0 {
		c.company.Store(cfg.CompanyID)
	}
	return c
}

// SetCookie installs a new cookie header value atomically.
func (c *Client) SetCookie(v string) {
	c.cookie.Store(strings.TrimSpace(v))
}

// Cookie returns the current cookie value.
func (c *Client) Cookie() string {
	v, _ := c.cookie.Load().(string)
	return v
}

// UA returns the User-Agent used for API and download requests.
func (c *Client) UA() string { return c.ua }

// StoreClient returns the long-timeout HTTP client used for byte transfers
// (storage uploads and proxied downloads). Its transport is connection-pooled.
func (c *Client) StoreClient() *http.Client { return c.store }

func (c *Client) driveHost() string {
	if c.mode == config.ModePersonal {
		return endpointPersonal
	}
	return endpointBusiness
}

func (c *Client) drivePrefix() string {
	if c.mode == config.ModePersonal {
		return ""
	}
	return prefixBusiness
}

func (c *Client) driveURL(path string) string {
	if c.endpoint != "" {
		return c.endpoint + path
	}
	return c.driveHost() + c.drivePrefix() + path
}

// CompanyID returns the discovered company id (business mode).
func (c *Client) CompanyID() int64 { return c.company.Load() }

// CheckLogin calls islogin and records the account state.
func (c *Client) CheckLogin(ctx context.Context) (LoginState, error) {
	var st LoginState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginCheckURL, nil)
	if err != nil {
		return st, err
	}
	c.setAPIHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return st, fmt.Errorf("islogin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("islogin: http %d (cookie expired?)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("islogin: unexpected response: %w", err)
	}
	if st.Result != "" && st.Result != "ok" {
		return st, fmt.Errorf("islogin: %s", st.Msg)
	}
	if st.CurrentCompanyID > 0 && c.company.Load() == 0 {
		c.company.Store(st.CurrentCompanyID)
	} else if st.CompanyID > 0 && c.company.Load() == 0 {
		c.company.Store(st.CompanyID)
	}
	c.IsLogin = st
	return st, nil
}

// Groups lists the drives visible to the account.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	if c.mode == config.ModePersonal {
		var r personalGroupsResp
		if err := c.getJSON(ctx, c.driveURL("/api/v3/groups"), &r); err != nil {
			return nil, err
		}
		out := make([]Group, 0, len(r.Groups))
		for _, g := range r.Groups {
			out = append(out, Group{GroupID: g.ID, Name: g.Name})
		}
		return out, nil
	}
	company := c.CompanyID()
	if company == 0 {
		if _, err := c.CheckLogin(ctx); err != nil {
			return nil, err
		}
		company = c.CompanyID()
	}
	if company == 0 {
		return nil, fmt.Errorf("business mode: company id unknown; set company_id in config")
	}
	u := fmt.Sprintf("%s/3rd/plus/groups/v1/companies/%d/users/self/groups/private", endpointBusiness, company)
	var r businessGroupsResp
	if err := c.getJSON(ctx, u, &r); err != nil {
		return nil, err
	}
	return r.Groups, nil
}

// SpecialGroup discovers the personal "my docs" group.
func (c *Client) SpecialGroup(ctx context.Context) (int64, error) {
	var r specialGroupResp
	if err := c.getJSON(ctx, c.driveURL("/api/v3/groups/special"), &r); err != nil {
		return 0, err
	}
	if r.ID == 0 {
		return 0, fmt.Errorf("special group not found: %s", r.Msg)
	}
	return r.ID, nil
}

// Quota reports total/used bytes for the account.
func (c *Client) Quota(ctx context.Context) (total, used int64, err error) {
	if c.mode == config.ModePersonal {
		var r spacesResp
		if err := c.getJSON(ctx, c.driveURL("/api/v3/spaces"), &r); err != nil {
			return 0, 0, err
		}
		return r.Total, r.Used, nil
	}
	company := c.CompanyID()
	u := fmt.Sprintf("%s/3rd/plussvr/compose/v1/u/companies/batch/service-space?comp_ids=%d", endpointBusiness, company)
	var r serviceSpaceResp
	if err := c.getJSON(ctx, u, &r); err != nil {
		return 0, 0, err
	}
	for _, it := range r.Info {
		if it.ID == company || len(r.Info) == 1 {
			return it.SpaceTotal, it.SpaceUsed, nil
		}
	}
	return 0, 0, nil
}

// ListAll lists every entry under parentID (0 = group root), following
// the offset/next_offset pagination until exhaustion.
func (c *Client) ListAll(ctx context.Context, groupID, parentID int64) ([]FileInfo, error) {
	const pageSize = 200
	var out []FileInfo
	offset := 0
	for page := 0; page < 500; page++ {
		u := fmt.Sprintf("%s?parentid=%d&offset=%d&count=%d",
			c.driveURL(fmt.Sprintf("/api/v5/groups/%d/files", groupID)), parentID, offset, pageSize)
		var r filesResp
		if err := c.getJSON(ctx, u, &r); err != nil {
			return nil, err
		}
		if r.Result != "" && r.Result != "ok" {
			return nil, fmt.Errorf("list files: %s", r.Msg)
		}
		out = append(out, r.Files...)
		if r.NextOffset < 0 || len(r.Files) == 0 {
			return out, nil
		}
		offset = r.NextOffset
	}
	return out, fmt.Errorf("list files: pagination runaway (group %d parent %d)", groupID, parentID)
}

// DownloadURL returns a presigned URL for the given file.
func (c *Client) DownloadURL(ctx context.Context, groupID, fileID int64) (string, error) {
	u := fmt.Sprintf("%s/download?support_checksums=sha1",
		c.driveURL(fmt.Sprintf("/api/v5/groups/%d/files/%d", groupID, fileID)))
	var r downloadResp
	if err := c.getJSON(ctx, u, &r); err != nil {
		return "", err
	}
	if r.URL == "" {
		if r.Result != "" && r.Result != "ok" {
			return "", fmt.Errorf("download url: %s", r.Msg)
		}
		return "", fmt.Errorf("download url: empty url")
	}
	return r.URL, nil
}

// ResolveDownloadURL also returns the headers the storage CDN may require.
func (c *Client) ResolveDownloadURL(ctx context.Context, groupID, fileID int64) (string, http.Header, error) {
	u, err := c.DownloadURL(ctx, groupID, fileID)
	if err != nil {
		return "", nil, err
	}
	h := make(http.Header)
	h.Set("User-Agent", c.ua)
	h.Set("Referer", c.driveHost())
	return u, h, nil
}

// getJSON issues an authenticated GET and decodes JSON into out.
// Transient network/TLS failures are retried (WPS endpoints occasionally
// reset connections).
func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		err := c.getJSONOnce(ctx, u, out)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// A real HTTP response: only retry server-side failures.
			if apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
		if !sleepCtx(ctx, backoff(attempt)) {
			return ctx.Err()
		}
	}
	return last
}

func (c *Client) getJSONOnce(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.setAPIHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{Op: "GET " + redact(u), Status: resp.StatusCode, Body: snippet(body)}
	}
	return json.Unmarshal(body, out)
}

// doJSON issues an authenticated request with a JSON body and returns raw bytes.
func (c *Client) doJSON(ctx context.Context, method, u string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	c.setAPIHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return data, resp.StatusCode, err
}

// doJSONResult performs a mutating call with retry on the WPS
// "fileTaskDuplicated" race (403) and transient errors.
func (c *Client) doJSONResult(ctx context.Context, method, u string, body any) (apiResult, error) {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		data, status, err := c.doJSON(ctx, method, u, body)
		if err != nil {
			last = err
			if !sleepCtx(ctx, backoff(attempt)) {
				return apiResult{}, ctx.Err()
			}
			continue
		}
		var r apiResult
		_ = json.Unmarshal(data, &r)
		if status == http.StatusOK && (r.Result == "" || r.Result == "ok") {
			return r, nil
		}
		if status == http.StatusForbidden && r.Result == "fileTaskDuplicated" {
			// Folders cannot be created/deleted back-to-back; WPS asks for a pause.
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return apiResult{}, ctx.Err()
			}
			continue
		}
		last = &APIError{Op: method + " " + redact(u), Status: status, Result: r.Result, Msg: r.Msg}
		if status >= 500 || status == http.StatusTooManyRequests {
			if !sleepCtx(ctx, backoff(attempt)) {
				return apiResult{}, ctx.Err()
			}
			continue
		}
		return r, last
	}
	if last == nil {
		last = fmt.Errorf("request failed")
	}
	return apiResult{}, last
}

func (c *Client) setAPIHeaders(req *http.Request) {
	req.Header.Set("Cookie", c.Cookie())
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Referer", c.driveHost()+"/")
	req.Header.Set("Origin", c.driveHost())
	req.Header.Set("Accept", "application/json, text/plain, */*")
}

// respArg resolves a plan extraction spec: "header.Name" or "body.key".
func respArg(spec string, header http.Header, body []byte) string {
	if spec == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(spec, "header."):
		return header.Get(strings.TrimPrefix(spec, "header."))
	case strings.HasPrefix(spec, "body."):
		key := strings.TrimPrefix(spec, "body.")
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(250*time.Millisecond)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// redact hides presigned query strings in logs/errors.
func redact(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.RawQuery != "" {
		parsed.RawQuery = "…"
		return parsed.String()
	}
	return u
}
