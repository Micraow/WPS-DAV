package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"wpsdav/internal/wps"
)

func TestErrorRewriterUpstreamPassthrough(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "upload failure becomes 502",
			code:       http.StatusMethodNotAllowed,
			err:        &wps.APIError{Op: "store put", Status: http.StatusServiceUnavailable},
			wantStatus: http.StatusBadGateway,
			wantBody:   "store put",
		},
		{
			name:       "transport failure becomes 502",
			code:       http.StatusMethodNotAllowed,
			err:        &wps.APIError{Op: "store put", Err: errors.New("EOF")},
			wantStatus: http.StatusBadGateway,
			wantBody:   "EOF",
		},
		{
			name:       "rate limit passes 429 and Retry-After",
			code:       http.StatusMethodNotAllowed,
			err:        &wps.APIError{Status: http.StatusTooManyRequests, Msg: "您使用太频繁了，请稍等"},
			wantStatus: http.StatusTooManyRequests,
			wantBody:   "太频繁",
		},
		{
			name:       "directory exists stays 405 for rclone",
			code:       http.StatusMethodNotAllowed,
			err:        os.ErrExist,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "missing file stays 404",
			code:       http.StatusNotFound,
			err:        os.ErrNotExist,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "listing failure behind 404 becomes 502",
			code:       http.StatusNotFound,
			err:        &wps.APIError{Op: "GET list", Err: errors.New("unexpected EOF")},
			wantStatus: http.StatusBadGateway,
			wantBody:   "unexpected EOF",
		},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		w := &errorRewriter{ResponseWriter: rec, rec: &davError{err: tc.err}}
		w.WriteHeader(tc.code)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.wantStatus)
		}
		if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: body = %q, want substring %q", tc.name, rec.Body.String(), tc.wantBody)
		}
		if tc.wantStatus == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: missing Retry-After", tc.name)
		}
	}
}
