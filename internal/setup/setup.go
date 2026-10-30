// Package setup contains reusable configuration and credential helpers shared
// by the CLI and the interactive TUI.
package setup

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"wpsdav/internal/config"
	"wpsdav/internal/sso"
)

// UserConfigPath returns ~/.config/wpsdav/config.json.
func UserConfigPath() string {
	return filepath.Join(config.UserConfigDir(), "wpsdav", "config.json")
}

// Init creates a config file. When path is empty the standard user (or system)
// location is used.
func Init(path string, system, buaa bool, listen string, force bool) (*config.Config, string, error) {
	if path == "" {
		if system {
			path = "/etc/wpsdav/config.json"
		} else {
			path = UserConfigPath()
		}
	}
	if _, err := os.Stat(path); err == nil && !force {
		return nil, "", fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}
	password, err := randomPassword(20)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(path)
	cfg := config.Default()
	cfg.Listen = listen
	cfg.Username = "wpsdav"
	cfg.Password = password
	cfg.SourcePath = path
	if buaa {
		cfg.Auth.Mode = "buaa"
	}
	if system {
		cfg.CookieFile = "/var/lib/wpsdav/cookie.txt"
	} else {
		cfg.CookieFile = filepath.Join(config.UserStateDir(), "wpsdav", "cookie.txt")
	}
	cfg.Upload.AgentAPIKeyFile = filepath.Join(dir, "agent.key")
	for _, d := range []string{dir, filepath.Dir(cfg.CookieFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, "", fmt.Errorf("create %s: %w", d, err)
		}
	}
	if err := config.Save(path, cfg); err != nil {
		return nil, "", err
	}
	return cfg, password, nil
}

// SaveAgentKey stores an AGENT_API_KEY (0600) and persists the config. It
// deliberately does not change the selected upload backend.
func SaveAgentKey(cfg *config.Config, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty agent key")
	}
	if !strings.HasPrefix(key, "apik:") {
		return fmt.Errorf("agent key must start with \"apik:\"")
	}
	if cfg.Upload.AgentAPIKeyFile == "" {
		cfg.Upload.AgentAPIKeyFile = filepath.Join(filepath.Dir(cfg.SourcePath), "agent.key")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Upload.AgentAPIKeyFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.Upload.AgentAPIKeyFile, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	cfg.Upload.AgentAPIKey = ""
	if cfg.SourcePath != "" {
		return config.Save(cfg.SourcePath, cfg)
	}
	return nil
}

// SaveCookie writes the cookie header to the configured file.
func SaveCookie(cfg *config.Config, header string) error {
	if err := os.MkdirAll(filepath.Dir(cfg.CookieFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(cfg.CookieFile, []byte(strings.TrimSpace(header)+"\n"), 0o600)
}

// LoginBuaa performs a BUAA SSO login and stores the resulting cookie.
func LoginBuaa(ctx context.Context, cfg *config.Config, username, password string) (*sso.Session, error) {
	entry := cfg.Auth.SSO.EntryURL
	manager := sso.NewManager(username, password, entry)
	sess, err := manager.Login(ctx)
	if err != nil {
		return nil, err
	}
	if err := SaveCookie(cfg, sess.CookieHeader()); err != nil {
		return nil, err
	}
	cfg.Auth.Mode = "buaa"
	cfg.Auth.SSO.Username = username
	if cfg.SourcePath != "" {
		_ = config.Save(cfg.SourcePath, cfg)
	}
	return sess, nil
}

func randomPassword(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}
