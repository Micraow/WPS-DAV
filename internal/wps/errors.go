package wps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIError is an upstream failure that can be classified by HTTP status.
// Status is zero when no response was received (DNS/TLS/connection failure).
type APIError struct {
	Op         string
	Status     int
	Result     string
	Msg        string
	Body       string
	RetryAfter time.Duration
	Err        error
}

func (e *APIError) Error() string {
	var parts []string
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("http %d", e.Status))
	}
	if e.Result != "" {
		parts = append(parts, fmt.Sprintf("result=%q", e.Result))
	}
	if e.Msg != "" {
		parts = append(parts, fmt.Sprintf("msg=%q", e.Msg))
	}
	if e.Body != "" {
		parts = append(parts, e.Body)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	if len(parts) == 0 {
		return "upstream error"
	}
	return strings.Join(parts, ": ")
}

func (e *APIError) Unwrap() error { return e.Err }

// transportError wraps a network failure. The URL is redacted because store
// URLs carry presigned credentials.
func transportError(op, rawURL string, err error) *APIError {
	if ue, ok := err.(*url.Error); ok {
		display := rawURL
		if display == "" {
			display = ue.URL
		}
		err = &url.Error{Op: ue.Op, URL: redact(display), Err: ue.Err}
	}
	return &APIError{Op: op, Err: err}
}

func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryable reports whether an upload stage is worth retrying. Rate limits,
// server-side failures, and transport errors are transient; client errors
// (bad name, missing parent, permission denied) are not.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) ||
		errors.Is(err, os.ErrInvalid) || errors.Is(err, os.ErrPermission) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status == 0 {
			// No HTTP response: connection reset, EOF, TLS/proxy failure.
			return ae.Err != nil
		}
		switch ae.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return true
		}
		if ae.Status >= 500 {
			return true
		}
		// WPS uses 403/fileTaskDuplicated as a "try again shortly" signal.
		return ae.Result == "fileTaskDuplicated"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func isRateLimited(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == http.StatusTooManyRequests {
		return true
	}
	low := strings.ToLower(ae.Result + " " + ae.Msg)
	return strings.Contains(low, "ratelimit") || strings.Contains(low, "too many")
}

// HTTPStatus maps an upload error to a WebDAV status. ok is false when the
// error is a genuine client-side condition (not found / already exists) that
// the caller should keep as-is.
func HTTPStatus(err error) (status int, retryAfter time.Duration, ok bool) {
	if err == nil {
		return 0, 0, false
	}
	if errors.Is(err, context.Canceled) {
		return 0, 0, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, 0, true
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) {
		return 0, 0, false
	}
	if errors.Is(err, os.ErrPermission) {
		return http.StatusForbidden, 0, true
	}
	if errors.Is(err, os.ErrInvalid) {
		return http.StatusBadRequest, 0, true
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusTooManyRequests:
			wait := ae.RetryAfter
			if wait <= 0 {
				wait = 30 * time.Second
			}
			return http.StatusTooManyRequests, wait, true
		case ae.Status == 0:
			return http.StatusBadGateway, 0, true
		case ae.Status == http.StatusUnauthorized:
			// The WebDAV client's own credentials are fine; the upstream
			// cookie is stale. Report a gateway error so it is retried after
			// the credential supervisor renews the session.
			return http.StatusBadGateway, 0, true
		case ae.Status >= 500:
			return http.StatusBadGateway, 0, true
		case ae.Status == http.StatusNotFound:
			return http.StatusNotFound, 0, true
		case ae.Status == http.StatusConflict:
			return http.StatusConflict, 0, true
		case ae.Status == http.StatusInsufficientStorage:
			return http.StatusInsufficientStorage, 0, true
		case ae.Status == http.StatusForbidden:
			low := strings.ToLower(ae.Result + " " + ae.Msg)
			if strings.Contains(low, "login") || strings.Contains(low, "auth") ||
				strings.Contains(low, "cookie") {
				return http.StatusBadGateway, 0, true
			}
			return http.StatusForbidden, 0, true
		default:
			if ae.Status >= 400 {
				return ae.Status, 0, true
			}
			return http.StatusBadGateway, 0, true
		}
	}
	return http.StatusBadGateway, 0, true
}
