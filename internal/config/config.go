// Package config loads the wpsdav service configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects which WPS product the middle layer talks to.
type Mode string

const (
	// ModePersonal talks to the consumer WPS netdisk (drive.kdocs.cn).
	ModePersonal Mode = "personal"
	// ModeBusiness talks to WPS 365 (365.kdocs.cn, /3rd/drive prefix).
	ModeBusiness Mode = "business"
)

// DownloadMode controls whether GET is answered with a redirect to the
// presigned storage URL or proxied through this process.
type DownloadMode string

const (
	// DownloadRedirect sends a 302 so bulk traffic never touches this layer.
	DownloadRedirect DownloadMode = "redirect"
	// DownloadProxy streams bytes through this process (fallback when the
	// storage CDN rejects non-browser clients).
	DownloadProxy DownloadMode = "proxy"
)

// Config is the on-disk service configuration.
type Config struct {
	Mode Mode `json:"mode"`

	// Listen is the HTTP listen address, e.g. "127.0.0.1:8705".
	Listen string `json:"listen"`

	// DAVPath is the mount point of the WebDAV handler, e.g. "/dav".
	DAVPath string `json:"dav_path"`

	// Username/Password protect the WebDAV endpoint with basic auth.
	Username string `json:"username"`
	Password string `json:"password"`

	// CookieFile stores the browser cookie header value used for API calls.
	// In SSO mode it is written automatically after each login.
	CookieFile string `json:"cookie_file"`

	// Auth selects how cookies are obtained: "cookie" (manual) or "buaa"
	// (BUAA unified SSO via wps.buaa.edu.cn).
	Auth AuthConfig `json:"auth"`

	// RootGroupID optionally pins the group mounted at "/". Zero means:
	// personal mode auto-discovers the special group; business mode requires it.
	RootGroupID int64 `json:"root_group_id"`

	// CompanyID is only used in business mode. Zero means: read it from
	// the islogin response (current_companyid/companyid).
	CompanyID int64 `json:"company_id"`

	// DownloadMode is "redirect" (default) or "proxy".
	DownloadMode DownloadMode `json:"download_mode"`

	// CacheTTLSeconds is the directory listing cache lifetime.
	CacheTTLSeconds int `json:"cache_ttl_seconds"`

	// CustomUA overrides the User-Agent sent to the WPS API and downloaded
	// links. Empty means a Chrome-like UA.
	CustomUA string `json:"custom_ua"`

	// SpoolDir is where uploads are buffered (they must be hashed before the
	// create_update call). Empty means the system temp dir.
	SpoolDir string `json:"spool_dir"`

	// LogFile optionally tees logs to an append-only file. Empty means stderr
	// only (journald when running under systemd).
	LogFile string `json:"log_file"`

	// UploadRetries is the number of additional attempts after a failed
	// upload stage (create_update, store PUT, commit). The default is 5.
	// Zero disables retries.
	UploadRetries int `json:"upload_retries"`

	// UploadRetryBackoffSeconds is the base delay between upload retries.
	// Delays grow exponentially up to 30s and carry equal jitter. The default
	// is 2 seconds. Zero disables the delay (but keeps the retries).
	UploadRetryBackoffSeconds float64 `json:"upload_retry_backoff_seconds"`

	// Upload selects how file bytes are stored.
	Upload UploadConfig `json:"upload"`

	// SourcePath is the file this config was loaded from (not serialized).
	SourcePath string `json:"-"`
}

// Upload backend names.
const (
	UploadPrivate = "private"
	UploadV7      = "v7"
)

// UploadConfig selects and configures the upload backend.
type UploadConfig struct {
	// Backend is "private" (default, cookie API) or "v7" (official API).
	Backend string `json:"backend"`

	// AgentAPIKey is an inline AGENT_API_KEY (discouraged; prefer a file).
	AgentAPIKey string `json:"agent_api_key"`

	// AgentAPIKeyFile holds the AGENT_API_KEY (0600 recommended).
	AgentAPIKeyFile string `json:"agent_api_key_file"`

	// Fallback retries with the private backend when v7 fails (default true).
	Fallback bool `json:"fallback"`

	// MultipartThresholdBytes enables multipart uploads at or above this size.
	// Zero disables multipart. Default 64 MiB.
	MultipartThresholdBytes int64 `json:"multipart_threshold_bytes"`

	// SkipIdentical skips the upload when the target file already has the
	// same SHA-1 (default true).
	SkipIdentical bool `json:"skip_identical"`
}

// AuthConfig selects and configures the credential source.
type AuthConfig struct {
	// Mode is "cookie" (default) or "buaa".
	Mode string    `json:"mode"`
	SSO  SSOConfig `json:"sso"`
}

// SSOConfig configures the optional BUAA unified login layer.
type SSOConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// EntryURL defaults to https://wps.buaa.edu.cn/.
	EntryURL string `json:"entry_url"`
	// RefreshMinutes is how often to renew the session (grant_token first,
	// full login if that fails). Default 240 (sid lives ~3 days).
	RefreshMinutes int `json:"refresh_minutes"`
}

// Default returns the baseline configuration.
func Default() *Config {
	return &Config{
		Mode:            ModePersonal,
		Listen:          "127.0.0.1:8705",
		DAVPath:         "/dav",
		DownloadMode:    DownloadRedirect,
		CacheTTLSeconds: 60,

		UploadRetries:             5,
		UploadRetryBackoffSeconds: 2,

		Upload: UploadConfig{
			Backend:                 UploadPrivate,
			AgentAPIKeyFile:         "agent.key",
			Fallback:                true,
			MultipartThresholdBytes: 64 << 20,
			SkipIdentical:           true,
		},

		Auth: AuthConfig{
			Mode: "cookie",
			SSO: SSOConfig{
				EntryURL:       "https://wps.buaa.edu.cn/",
				RefreshMinutes: 240,
			},
		},
	}
}

// SearchPaths lists config file candidates in priority order (after the
// explicit --config flag and WPSDAV_CONFIG environment variable).
func SearchPaths() []string {
	paths := []string{"wpsdav.json", "config.json"}
	paths = append(paths, filepath.Join(UserConfigDir(), "wpsdav", "config.json"))
	paths = append(paths, "/etc/wpsdav/config.json")
	return paths
}

// UserConfigDir returns $XDG_CONFIG_HOME or ~/.config.
func UserConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config")
	}
	return "."
}

// UserStateDir returns $XDG_STATE_HOME or ~/.local/state.
func UserStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state")
	}
	return "."
}

// Load reads the JSON config at path, applying defaults for missing fields.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.SourcePath = path
	cfg.resolvePaths(path)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolvePaths fills defaults and resolves relative paths against the config
// file's directory. Without an explicit cookie_file, a user config stores
// state under XDG_STATE_HOME and a system config under /var/lib/wpsdav.
func (c *Config) resolvePaths(configPath string) {
	dir := filepath.Dir(configPath)
	system := filepath.Clean(dir) == "/etc/wpsdav"
	if c.CookieFile == "" {
		switch {
		case system:
			c.CookieFile = "/var/lib/wpsdav/cookie.txt"
		default:
			c.CookieFile = filepath.Join(UserStateDir(), "wpsdav", "cookie.txt")
		}
	} else if !filepath.IsAbs(c.CookieFile) {
		c.CookieFile = filepath.Join(dir, c.CookieFile)
	}
	if c.SpoolDir != "" && !filepath.IsAbs(c.SpoolDir) {
		c.SpoolDir = filepath.Join(dir, c.SpoolDir)
	}
	if c.LogFile != "" && !filepath.IsAbs(c.LogFile) {
		c.LogFile = filepath.Join(dir, c.LogFile)
	}
	if c.Upload.AgentAPIKeyFile != "" && !filepath.IsAbs(c.Upload.AgentAPIKeyFile) {
		c.Upload.AgentAPIKeyFile = filepath.Join(dir, c.Upload.AgentAPIKeyFile)
	}
}

// Validate checks the configuration for inconsistencies.
func (c *Config) Validate() error {
	if c.Mode != ModePersonal && c.Mode != ModeBusiness {
		return fmt.Errorf("mode must be %q or %q", ModePersonal, ModeBusiness)
	}
	if c.Listen == "" {
		return errors.New("listen is required")
	}
	if c.DAVPath == "" {
		c.DAVPath = "/dav"
	}
	if !strings.HasPrefix(c.DAVPath, "/") {
		return errors.New("dav_path must start with /")
	}
	if c.CookieFile == "" {
		return errors.New("cookie_file is required")
	}
	switch c.DownloadMode {
	case DownloadRedirect, DownloadProxy:
	case "":
		c.DownloadMode = DownloadRedirect
	default:
		return fmt.Errorf("download_mode must be %q or %q", DownloadRedirect, DownloadProxy)
	}
	if c.CacheTTLSeconds <= 0 {
		c.CacheTTLSeconds = 60
	}
	if c.UploadRetries < 0 {
		return errors.New("upload_retries must be >= 0")
	}
	if c.UploadRetryBackoffSeconds < 0 {
		return errors.New("upload_retry_backoff_seconds must be >= 0")
	}
	if c.Mode == ModeBusiness && c.RootGroupID == 0 {
		return errors.New("business mode requires root_group_id (run `wpsdav groups` to list them)")
	}
	if c.Upload.MultipartThresholdBytes < 0 {
		return errors.New("upload.multipart_threshold_bytes must be >= 0")
	}
	switch c.Upload.Backend {
	case "":
		c.Upload.Backend = UploadPrivate
	case UploadPrivate, UploadV7:
	default:
		return fmt.Errorf("upload.backend must be %q or %q", UploadPrivate, UploadV7)
	}
	switch c.Auth.Mode {
	case "", "cookie":
		c.Auth.Mode = "cookie"
	case "buaa":
		if c.Auth.SSO.EntryURL == "" {
			c.Auth.SSO.EntryURL = "https://wps.buaa.edu.cn/"
		}
		if c.Auth.SSO.RefreshMinutes <= 0 {
			c.Auth.SSO.RefreshMinutes = 240
		}
	default:
		return fmt.Errorf("auth.mode must be %q or %q", "cookie", "buaa")
	}
	return nil
}

// AgentKey resolves the AGENT_API_KEY: environment, inline config, then file.
func (c *Config) AgentKey() string {
	if v := strings.TrimSpace(os.Getenv("WPSDAV_AGENT_API_KEY")); v != "" {
		return v
	}
	if v := strings.TrimSpace(c.Upload.AgentAPIKey); v != "" {
		return v
	}
	if c.Upload.AgentAPIKeyFile != "" {
		if data, err := os.ReadFile(c.Upload.AgentAPIKeyFile); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// HasAgentKey reports whether an agent key is configured (without loading it
// into logs).
func (c *Config) HasAgentKey() bool { return c.AgentKey() != "" }

// Save writes the config back to path with 0600 permissions.
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
