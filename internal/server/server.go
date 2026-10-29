// Package server exposes the kvfs WebDAV view and control endpoints.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"wpsdav/internal/config"
	"wpsdav/internal/kvfs"
	"wpsdav/internal/ui"
	"wpsdav/internal/wps"
)

// Server wires the WPS client, the cache and the WebDAV handler together.
type Server struct {
	cfg    *config.Config
	client *wps.Client
	tree   *kvfs.Tree
	fs     *kvfs.FS
	dav    *webdav.Handler

	bufPool sync.Pool
}

// New builds the HTTP server.
func New(cfg *config.Config, client *wps.Client, tree *kvfs.Tree) *Server {
	fs := &kvfs.FS{Tree: tree, Client: client, SpoolDir: cfg.SpoolDir}
	s := &Server{
		cfg:    cfg,
		client: client,
		tree:   tree,
		fs:     fs,
		bufPool: sync.Pool{New: func() any {
			b := make([]byte, 64<<10)
			return &b
		}},
	}
	s.dav = &webdav.Handler{
		Prefix:     cfg.DAVPath,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger:     s.logDAV,
	}
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.withAuth(s.handleHealth))
	mux.HandleFunc("/admin/cookie", s.withAuth(s.handleCookie))
	mux.HandleFunc("/admin/refresh", s.withAuth(s.handleRefresh))
	mux.HandleFunc(s.cfg.DAVPath, s.withAuth(s.handleDAV))
	mux.HandleFunc(s.cfg.DAVPath+"/", s.withAuth(s.handleDAV))
	return mux
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	userHash := sha256Hex(s.cfg.Username)
	passHash := sha256Hex(s.cfg.Password)
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(sha256Hex(u)), []byte(userHash)) != 1 ||
			subtle.ConstantTimeCompare([]byte(sha256Hex(p)), []byte(passHash)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="wpsdav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleDAV(w http.ResponseWriter, r *http.Request) {
	// x/net/webdav maps every upload failure to 405 and any non-ENOENT open
	// failure to 404. Capture the real error and rewrite those two statuses
	// so clients see the upstream failure (and retry 5xx/429).
	rec := &davError{}
	r = r.WithContext(context.WithValue(r.Context(), davErrorKey{}, rec))
	w = &errorRewriter{ResponseWriter: w, rec: rec}

	if strings.HasPrefix(r.URL.Path, s.cfg.DAVPath) {
		p := strings.TrimPrefix(r.URL.Path, s.cfg.DAVPath)

		// Downloads never traverse this process in redirect mode: answer
		// GET/HEAD for regular files with a 302 to the presigned storage URL.
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			node, err := s.tree.Stat(r.Context(), p)
			if err == nil && !node.IsDir {
				s.serveFile(w, r, node)
				return
			}
		}

		// A failed listing must not masquerade as an empty directory: an
		// expired cookie would otherwise let clients "see" zero files.
		if r.Method == "PROPFIND" && r.Header.Get("Depth") != "0" {
			if node, err := s.tree.Stat(r.Context(), p); err == nil && node.IsDir {
				if _, err := s.tree.List(r.Context(), p); err != nil {
					status := http.StatusBadGateway
					if st, _, ok := upstreamStatus(err); ok {
						status = st
					}
					http.Error(w, "listing: "+err.Error(), status)
					return
				}
			}
		}

		// x/net/webdav implements COPY as read+write, which cannot work here
		// (reads are redirected at the HTTP layer). Use WPS server-side copy.
		if r.Method == "COPY" {
			s.handleCopy(w, r, p)
			return
		}
	}
	s.dav.ServeHTTP(w, r)
}

type davErrorKey struct{}

type davError struct{ err error }

// logDAV keeps the diagnostic log and records the error on the request so
// errorRewriter can map the status x/net/webdav chose.
func (s *Server) logDAV(r *http.Request, err error) {
	if err == nil {
		return
	}
	log.Printf("dav %s %s: %v", r.Method, r.URL.Path, err)
	if rec, ok := r.Context().Value(davErrorKey{}).(*davError); ok {
		rec.err = err
	}
}

// errorRewriter replaces the two catch-all statuses x/net/webdav emits with
// the status the upstream error deserves, and writes the real message.
type errorRewriter struct {
	http.ResponseWriter
	rec         *davError
	wroteHeader bool
	replaced    bool
}

func (w *errorRewriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if w.rec != nil && w.rec.err != nil &&
		(code == http.StatusNotFound || code == http.StatusMethodNotAllowed) {
		if status, retryAfter, ok := upstreamStatus(w.rec.err); ok {
			h := w.Header()
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Del("Content-Length")
			if retryAfter > 0 {
				h.Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.5)))
			}
			w.ResponseWriter.WriteHeader(status)
			fmt.Fprintf(w.ResponseWriter, "%s\n", w.rec.err)
			w.replaced = true
			return
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *errorRewriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.replaced {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *errorRewriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// upstreamStatus layers filesystem sentinels over the wps classification.
func upstreamStatus(err error) (int, time.Duration, bool) {
	if errors.Is(err, kvfs.ErrReadOnly) {
		return http.StatusForbidden, 0, true
	}
	return wps.HTTPStatus(err)
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request, src string) {
	raw := r.Header.Get("Destination")
	if raw == "" {
		http.Error(w, "missing Destination header", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		http.Error(w, "bad Destination header: "+err.Error(), http.StatusBadRequest)
		return
	}
	destPath := u.Path
	if !strings.HasPrefix(destPath, s.cfg.DAVPath) {
		http.Error(w, "destination outside DAV root", http.StatusBadGateway)
		return
	}
	dst := strings.TrimPrefix(destPath, s.cfg.DAVPath)
	overwrite := r.Header.Get("Overwrite") != "F"

	existed := false
	if _, err := s.tree.Stat(r.Context(), dst); err == nil {
		existed = true
	}
	if err := s.fs.Copy(r.Context(), src, dst, overwrite); err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, os.ErrExist):
			status = http.StatusPreconditionFailed
		case errors.Is(err, os.ErrNotExist):
			status = http.StatusNotFound
		case errors.Is(err, kvfs.ErrReadOnly):
			status = http.StatusForbidden
		default:
			if st, _, ok := upstreamStatus(err); ok {
				status = st
			}
		}
		http.Error(w, err.Error(), status)
		return
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, node *kvfs.Node) {
	link, hdr, err := s.client.ResolveDownloadURL(r.Context(), node.GroupID, node.ID)
	if err != nil {
		http.Error(w, "download link: "+err.Error(), http.StatusBadGateway)
		return
	}
	if s.cfg.DownloadMode == config.DownloadRedirect {
		http.Redirect(w, r, link, http.StatusFound)
		return
	}
	s.proxyDownload(w, r, link, hdr)
}

func (s *Server) proxyDownload(w http.ResponseWriter, r *http.Request, link string, hdr http.Header) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, link, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k := range hdr {
		req.Header.Set(k, hdr.Get(k))
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	if ir := r.Header.Get("If-Range"); ir != "" {
		req.Header.Set("If-Range", ir)
	}
	resp, err := s.client.StoreClient().Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	bp := s.bufPool.Get().(*[]byte)
	defer s.bufPool.Put(bp)
	start := time.Now()
	n, _ := io.CopyBuffer(w, resp.Body, *bp)
	log.Printf("%s", ui.TransferLine("download", r.URL.Path, n, time.Since(start)))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type health struct {
		OK      bool           `json:"ok"`
		Mode    string         `json:"mode"`
		Login   wps.LoginState `json:"login"`
		Company int64          `json:"company_id,omitempty"`
		Error   string         `json:"error,omitempty"`
		Total   int64          `json:"quota_total,omitempty"`
		Used    int64          `json:"quota_used,omitempty"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out := health{Mode: string(s.cfg.Mode), Company: s.client.CompanyID()}
	st, err := s.client.CheckLogin(ctx)
	out.Login = st
	if err != nil {
		out.Error = err.Error()
	} else {
		out.OK = true
		if r.URL.Query().Get("quota") == "1" {
			if total, used, qerr := s.client.Quota(ctx); qerr == nil {
				out.Total, out.Used = total, used
			}
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleCookie(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cookie := strings.TrimSpace(string(body))
	if cookie == "" {
		http.Error(w, "empty cookie", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(s.cfg.CookieFile, []byte(cookie+"\n"), 0o600); err != nil {
		http.Error(w, "persist cookie: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.client.SetCookie(cookie)
	s.tree.ForcedRefresh()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := s.client.CheckLogin(ctx)
	if err != nil {
		writeJSON(w, map[string]any{"saved": true, "login_ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"saved": true, "login_ok": true, "account": st})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.tree.ForcedRefresh()
	writeJSON(w, map[string]any{"refreshed": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}
