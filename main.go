// Command wpsdav exposes a WPS cloud drive as WebDAV for rclone mounting.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/term"

	"wpsdav/internal/auth"
	"wpsdav/internal/config"
	"wpsdav/internal/kvfs"
	"wpsdav/internal/server"
	"wpsdav/internal/wps"
)

const envConfigFile = "WPSDAV_CONFIG"

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("wpsdav: ")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "groups":
		cmdGroups(os.Args[2:])
	case "cookie", "set-cookie":
		cmdCookie(os.Args[2:])
	case "login":
		cmdLogin(os.Args[2:])
	case "version", "--version":
		fmt.Printf("wpsdav %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `wpsdav - WPS cloud drive as WebDAV for rclone

usage:
  wpsdav serve     run the WebDAV/control server (config: ./wpsdav.json)
  wpsdav doctor    check credentials, listing, and download-URL behavior
  wpsdav groups    list drives (groups) visible to the account
  wpsdav cookie    install a cookie from an argument or stdin
  wpsdav login     [--provider buaa] [学号]  log in through an auth provider
  wpsdav version   print version information

Config discovery: ./wpsdav.json, then ./config.json (override with -c or
`+envConfigFile+`). SSO credentials fall back to WPSDAV_SSO_USERNAME /
WPSDAV_SSO_PASSWORD.
`)
}

// configPath resolves the config file: -c, env, then the current directory.
func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(envConfigFile); v != "" {
		return v
	}
	for _, p := range []string{"wpsdav.json", "config.json"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func loadConfig(fs *pflag.FlagSet) *config.Config {
	path := fs.StringP("config", "c", "", "config file (default: ./wpsdav.json or ./config.json)")
	_ = fs.Parse(os.Args[2:])
	cfgFile := configPath(*path)
	if cfgFile == "" {
		log.Fatalf("no config found: create ./wpsdav.json or pass -c (searched ./wpsdav.json, ./config.json)")
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	return cfg
}

func newClient(cfg *config.Config) *wps.Client {
	client := wps.New(cfg)
	if data, err := os.ReadFile(cfg.CookieFile); err == nil {
		cookie := strings.TrimSpace(string(data))
		if cookie != "" {
			client.SetCookie(cookie)
		}
	}
	return client
}

func persistCookie(cfg *config.Config, header string) {
	if err := os.WriteFile(cfg.CookieFile, []byte(header+"\n"), 0o600); err != nil {
		log.Printf("warning: persist cookie: %v", err)
	}
}

// seedProvider lets a login-capable provider adopt the cached cookie, so it
// can renew the session (grant_token) without stored credentials.
func seedProvider(prov auth.Provider, cookie string) {
	if cookie == "" {
		return
	}
	if s, ok := prov.(auth.Seedable); ok && s.Seed(cookie) {
		log.Printf("provider %s seeded with cached cookie", prov.Name())
	}
}

// ssoCredentials resolves SSO credentials from config, then environment.
func ssoCredentials(cfg *config.Config) (string, string) {
	user := strings.TrimSpace(cfg.Auth.SSO.Username)
	if user == "" {
		user = strings.TrimSpace(os.Getenv("WPSDAV_SSO_USERNAME"))
	}
	pass := cfg.Auth.SSO.Password
	if pass == "" {
		pass = os.Getenv("WPSDAV_SSO_PASSWORD")
	}
	return user, pass
}

// ensureCredentials validates the cached cookie and, when needed, asks the
// auth provider for a fresh one. It returns the resulting login state.
func ensureCredentials(ctx context.Context, cfg *config.Config, prov auth.Provider, client *wps.Client) (wps.LoginState, error) {
	if client.Cookie() != "" {
		if st, err := client.CheckLogin(ctx); err == nil {
			return st, nil
		}
		log.Printf("stored cookie rejected, renewing via %s", prov.Name())
	}
	cookie, err := prov.Renew(ctx)
	if err != nil {
		return wps.LoginState{}, err
	}
	client.SetCookie(cookie)
	persistCookie(cfg, cookie)
	return client.CheckLogin(ctx)
}

// superviseCredentials proactively renews credentials when the session dies
// (external logout, expired cookie, revoked SSO session).
func superviseCredentials(ctx context.Context, cfg *config.Config, prov auth.Provider, client *wps.Client, tree *kvfs.Tree) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	var lastLog time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := client.CheckLogin(checkCtx)
		if err == nil {
			cancel()
			continue
		}
		cookie, rerr := prov.Renew(checkCtx)
		cancel()
		if rerr != nil {
			if time.Since(lastLog) > 10*time.Minute {
				log.Printf("credentials invalid (%v); renew via %s unavailable: %v", err, prov.Name(), rerr)
				lastLog = time.Now()
			}
			continue
		}
		client.SetCookie(cookie)
		persistCookie(cfg, cookie)
		tree.ForcedRefresh()
		log.Printf("credentials renewed via %s", prov.Name())
	}
}

func cmdServe(args []string) {
	fs := pflag.NewFlagSet("serve", pflag.ExitOnError)
	cfg := loadConfig(fs)
	if cfg.Username == "" || cfg.Password == "" {
		log.Fatalf("username and password are required in the config (the DAV endpoint must not be open)")
	}

	client := newClient(cfg)
	prov, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	st, err := ensureCredentials(ctx, cfg, prov, client)
	if err != nil {
		if errors.Is(err, auth.ErrManual) || errors.Is(err, auth.ErrNoCredentials) {
			cancel()
			if client.Cookie() == "" {
				log.Fatalf("no usable credentials: %v\n  run `wpsdav cookie` (manual) or `wpsdav login -provider buaa` (SSO)", err)
			}
			log.Printf("warning: credentials check failed (%v); serving with the cached cookie until it expires", err)
		} else {
			cancel()
			log.Fatalf("authentication failed: %v", err)
		}
	} else {
		log.Printf("logged in: userid=%d company=%d", st.UserID, st.CurrentCompanyID)
	}

	rootID := cfg.RootGroupID
	if rootID == 0 {
		if cfg.Mode == config.ModePersonal {
			rootID, err = client.SpecialGroup(ctx)
		} else {
			err = fmt.Errorf("business mode needs root_group_id")
		}
		if err != nil {
			cancel()
			log.Fatalf("resolve root group: %v", err)
		}
	}
	cancel()
	log.Printf("mode=%s root_group=%d company=%d auth=%s", cfg.Mode, rootID, client.CompanyID(), prov.Name())

	tree := kvfs.NewTree(client, rootID, time.Duration(cfg.CacheTTLSeconds)*time.Second)
	srv := server.New(cfg, client, tree)

	bg, stopBG := context.WithCancel(context.Background())
	go prov.Run(bg, func(cookie string) {
		client.SetCookie(cookie)
		tree.ForcedRefresh()
	})
	go superviseCredentials(bg, cfg, prov, client, tree)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: uploads and proxied downloads may be long-lived.
	}

	go func() {
		log.Printf("listening on %s (dav path %s, download mode %s)", cfg.Listen, cfg.DAVPath, cfg.DownloadMode)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	stopBG()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Printf("bye")
}

func cmdLogin(args []string) {
	fs := pflag.NewFlagSet("login", pflag.ExitOnError)
	provider := fs.StringP("provider", "p", "", `auth provider to log in with (supported: "buaa" = 北航统一身份认证)`)
	cfg := loadConfig(fs)
	mode := strings.TrimSpace(*provider)
	if mode == "" {
		mode = cfg.Auth.Mode
	}
	if mode == "" {
		mode = "cookie"
	}
	if mode != "buaa" {
		log.Fatalf("provider %q does not support login; supported: buaa (北航统一身份认证); cookie credentials are installed with `wpsdav cookie`", mode)
	}
	user, pass := ssoCredentials(cfg)
	if rest := fs.Args(); len(rest) > 0 {
		user = strings.TrimSpace(rest[0])
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if user == "" && interactive {
		fmt.Print("学号: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		user = strings.TrimSpace(line)
	}
	if pass == "" && interactive {
		fmt.Print("密码（输入不回显）: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatalf("read password: %v", err)
		}
		pass = string(pw)
	}
	if user == "" || pass == "" {
		log.Fatalf("missing credentials: pass 学号 as an argument, or set sso.username/password in config, or %s/%s",
			"WPSDAV_SSO_USERNAME", "WPSDAV_SSO_PASSWORD")
	}

	prov := auth.NewBuaaProvider(user, pass, cfg.Auth.SSO.EntryURL, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cookie, err := prov.Renew(ctx)
	if err != nil {
		log.Fatalf("login via %s failed: %v", prov.Name(), err)
	}
	persistCookie(cfg, cookie)
	fmt.Printf("login via %s ok; cookie written to %s\n", prov.Name(), cfg.CookieFile)
}

func cmdGroups(args []string) {
	fs := pflag.NewFlagSet("groups", pflag.ExitOnError)
	cfg := loadConfig(fs)
	client := newClient(cfg)
	prov, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := ensureCredentials(ctx, cfg, prov, client); err != nil {
		log.Fatalf("no valid credentials: %v", err)
	}
	groups, err := client.Groups(ctx)
	if err != nil {
		log.Fatalf("groups: %v", err)
	}
	seen := make(map[int64]bool)
	if cfg.Mode == config.ModePersonal {
		if id, err := client.SpecialGroup(ctx); err == nil {
			seen[id] = true
			fmt.Printf("%-16d %s (special/default)\n", id, "我的企业文档")
		}
	}
	for _, g := range groups {
		if seen[g.GroupID] {
			continue
		}
		fmt.Printf("%-16d %s\n", g.GroupID, g.Name)
	}
}

func cmdCookie(args []string) {
	fs := pflag.NewFlagSet("cookie", pflag.ExitOnError)
	cfg := loadConfig(fs)
	value := strings.Join(fs.Args(), " ")
	if value == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("read stdin: %v", err)
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		log.Fatalf("no cookie provided (argument or stdin)")
	}
	if err := os.WriteFile(cfg.CookieFile, []byte(value+"\n"), 0o600); err != nil {
		log.Fatalf("write %s: %v", cfg.CookieFile, err)
	}
	fmt.Printf("cookie written to %s (%d bytes)\n", cfg.CookieFile, len(value))
}

func cmdDoctor(args []string) {
	fs := pflag.NewFlagSet("doctor", pflag.ExitOnError)
	cfg := loadConfig(fs)
	client := newClient(cfg)
	prov, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Println("== credentials ==")
	st, err := ensureCredentials(ctx, cfg, prov, client)
	if err != nil {
		fmt.Printf("FAIL no usable credentials: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK   provider=%s userid=%d company=%d company_account=%v\n",
		prov.Name(), st.UserID, st.CurrentCompanyID, st.IsCompanyAccount)

	fmt.Println("== groups ==")
	groups, err := client.Groups(ctx)
	if err != nil {
		fmt.Printf("FAIL groups: %v\n", err)
	} else {
		for _, g := range groups {
			fmt.Printf("     %-16d %s\n", g.GroupID, g.Name)
		}
	}

	rootID := cfg.RootGroupID
	if rootID == 0 && cfg.Mode == config.ModePersonal {
		rootID, err = client.SpecialGroup(ctx)
		if err != nil {
			fmt.Printf("FAIL special group: %v\n", err)
		}
	}
	if rootID == 0 {
		fmt.Println("SKIP listing (no root group resolved)")
		return
	}

	fmt.Println("== listing ==")
	files, err := client.ListAll(ctx, rootID, 0)
	if err != nil {
		fmt.Printf("FAIL list root: %v\n", err)
		return
	}
	fmt.Printf("OK   %d entries at root\n", len(files))
	var sample *wps.FileInfo
	for i := range files {
		f := &files[i]
		kind := "file"
		if f.IsDir() {
			kind = "dir "
		}
		fmt.Printf("     [%s] %-40s id=%-12d size=%d mtime=%s\n",
			kind, trunc(f.Name, 40), f.ID, f.Size, time.Unix(f.Mtime, 0).Format("2006-01-02 15:04"))
		if sample == nil && !f.IsDir() {
			sample = f
		}
	}
	if sample == nil {
		fmt.Println("SKIP download-URL test (no file at root)")
		return
	}

	fmt.Println("== download url ==")
	link, hdr, err := client.ResolveDownloadURL(ctx, sample.GroupID, sample.ID)
	if err != nil {
		fmt.Printf("FAIL get download url: %v\n", err)
		return
	}
	fmt.Printf("OK   link for %q\n", sample.Name)

	profiles := []struct {
		name    string
		headers map[string]string
		note    string
	}{
		{"rclone-like UA, no referer", map[string]string{"User-Agent": "rclone/v1.71.2"}, "redirect mode viable"},
		{"curl UA, no referer", map[string]string{"User-Agent": "curl/8.9.1"}, "redirect mode viable"},
		{"browser UA + referer", map[string]string{"User-Agent": client.UA(), "Referer": hdr.Get("Referer")}, "reference"},
		{"no UA at all", map[string]string{"User-Agent": ""}, "reference"},
	}
	redirectOK := false
	for _, p := range profiles {
		code, err := probe(link, p.headers)
		state := fmt.Sprintf("http %d", code)
		if err != nil {
			state = "error: " + err.Error()
		}
		mark := " "
		if code >= 200 && code < 400 {
			mark = "OK"
			if strings.Contains(p.name, "rclone-like") {
				redirectOK = true
			}
		}
		fmt.Printf("%-4s %-28s %-40s (%s)\n", mark, p.name, state, p.note)
	}
	if redirectOK {
		fmt.Println("\nrecommendation: download_mode=redirect — bulk traffic bypasses wpsdav")
	} else {
		fmt.Println("\nrecommendation: download_mode=proxy — the CDN rejects non-browser clients")
	}
}

func probe(link string, headers map[string]string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", "bytes=0-0")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	return resp.StatusCode, nil
}

func trunc(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
