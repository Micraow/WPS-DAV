// Command wpsdav exposes a WPS cloud drive as WebDAV for rclone mounting.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/term"

	"wpsdav/internal/agent"
	"wpsdav/internal/auth"
	"wpsdav/internal/config"
	"wpsdav/internal/kvfs"
	"wpsdav/internal/logging"
	"wpsdav/internal/server"
	"wpsdav/internal/service"
	"wpsdav/internal/tui"
	"wpsdav/internal/ui"
	"wpsdav/internal/wps"
	"wpsdav/internal/wpsv7"
)

const envConfigFile = "WPSDAV_CONFIG"

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "tui":
		cmdTUI(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "%s\n\n%s\n", ui.Bold("wpsdav - WPS cloud drive as WebDAV for rclone"), ui.Bold("usage:"))
	rows := [][2]string{
		{"init", "create a config file with a generated password"},
		{"serve", "run the WebDAV/control server"},
		{"tui", "interactive setup and operations menu"},
		{"service", "enable/disable/status of the systemd user unit"},
		{"doctor", "check credentials, listing, and download-URL behavior"},
		{"groups", "list drives (groups) visible to the account"},
		{"cookie", "install a cookie from an argument or stdin"},
		{"login", "[-p buaa] [学号]  log in through an auth provider"},
		{"version", "print version information"},
	}
	for _, row := range rows {
		fmt.Fprintf(os.Stderr, "  %s  %s\n", ui.Cyan(fmt.Sprintf("%-8s", row[0])), row[1])
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", ui.Bold("Config discovery (first match wins):"))
	fmt.Fprintln(os.Stderr, "  --config PATH, $WPSDAV_CONFIG, ./wpsdav.json, ./config.json,")
	fmt.Fprintln(os.Stderr, "  ~/.config/wpsdav/config.json, /etc/wpsdav/config.json")
	fmt.Fprintf(os.Stderr, "\nRun %s to create a user config with a generated password.\n", ui.Cyan("wpsdav init"))
	fmt.Fprintln(os.Stderr, "SSO credentials fall back to WPSDAV_SSO_USERNAME / WPSDAV_SSO_PASSWORD.")
}

// configPath resolves the config file: flag, env, then standard locations.
func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(envConfigFile); v != "" {
		return v
	}
	for _, p := range config.SearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func loadConfig(fs *pflag.FlagSet) *config.Config {
	path := fs.StringP("config", "c", "", "config file (default: search standard locations)")
	_ = fs.Parse(os.Args[2:])
	cfgFile := configPath(*path)
	if cfgFile == "" {
		searched := append([]string{"$" + envConfigFile}, config.SearchPaths()...)
		logging.Fatalf("no config found (searched: %s)\n  create one with `wpsdav init`, or pass --config PATH",
			strings.Join(searched, ", "))
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		logging.Fatalf("config %s: %v", cfgFile, err)
	}
	if ferr := logging.SetFile(cfg.LogFile); ferr != nil {
		logging.Warnf("log file disabled: %v", ferr)
	}
	logging.Infof("config: %s", ui.Dim(cfgFile))
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
		logging.Warnf("persist cookie: %v", err)
	}
}

// seedProvider lets a login-capable provider adopt the cached cookie, so it
// can renew the session (grant_token) without stored credentials.
func seedProvider(prov auth.Provider, cookie string) {
	if cookie == "" {
		return
	}
	if s, ok := prov.(auth.Seedable); ok && s.Seed(cookie) {
		logging.Infof("provider %s seeded with cached cookie", prov.Name())
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
		logging.Infof("stored cookie rejected, renewing via %s", prov.Name())
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
				logging.Infof("credentials invalid (%v); renew via %s unavailable: %v", err, prov.Name(), rerr)
				lastLog = time.Now()
			}
			continue
		}
		client.SetCookie(cookie)
		persistCookie(cfg, cookie)
		tree.ForcedRefresh()
		logging.Infof("credentials renewed via %s", prov.Name())
	}
}

// cmdInit writes a ready-to-edit config with a generated WebDAV password.
func cmdInit(args []string) {
	fs := pflag.NewFlagSet("init", pflag.ExitOnError)
	system := fs.Bool("system", false, "write /etc/wpsdav/config.json (requires root)")
	buaa := fs.Bool("buaa", false, "select the BUAA SSO provider")
	force := fs.Bool("force", false, "overwrite an existing config")
	listen := fs.String("listen", "127.0.0.1:8705", "listen address")
	_ = fs.Parse(os.Args[2:])

	var cfgPath, cookiePath string
	if *system {
		if os.Geteuid() != 0 {
			logging.Fatalf("--system requires root")
		}
		cfgPath = "/etc/wpsdav/config.json"
		cookiePath = "/var/lib/wpsdav/cookie.txt"
	} else {
		cfgPath = filepath.Join(config.UserConfigDir(), "wpsdav", "config.json")
		cookiePath = filepath.Join(config.UserStateDir(), "wpsdav", "cookie.txt")
	}
	if _, err := os.Stat(cfgPath); err == nil && !*force {
		logging.Fatalf("%s already exists (use --force to overwrite)", cfgPath)
	}

	password, err := randomPassword(20)
	if err != nil {
		logging.Fatalf("generate password: %v", err)
	}
	cfg := config.Default()
	cfg.Listen = *listen
	cfg.Username = "wpsdav"
	cfg.Password = password
	cfg.CookieFile = cookiePath
	if *buaa {
		cfg.Auth.Mode = "buaa"
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		logging.Fatalf("encode config: %v", err)
	}
	for _, dir := range []string{filepath.Dir(cfgPath), filepath.Dir(cookiePath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logging.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o600); err != nil {
		logging.Fatalf("write %s: %v", cfgPath, err)
	}

	fmt.Printf("%s %s (0600)\n", ui.Bold("config written:"), cfgPath)
	fmt.Printf("%s %s\n", ui.Bold("cookie file:   "), cookiePath)
	fmt.Printf("%s %s\n", ui.Bold("WebDAV user:   "), cfg.Username)
	fmt.Printf("%s %s\n", ui.Bold("WebDAV pass:   "), ui.Cyan(password))
	fmt.Printf("\n%s\n", ui.Bold("next steps:"))
	fmt.Println("  1. wpsdav login --provider buaa 学号   # 或 wpsdav cookie")
	fmt.Println("  2. wpsdav doctor")
	fmt.Println("  3. wpsdav serve")
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

// buildUploader selects the private or v7 upload backend.
func buildUploader(cfg *config.Config, client *wps.Client) (kvfs.Uploader, error) {
	private := &kvfs.PrivateUploader{Client: client}
	if cfg.Upload.Backend != config.UploadV7 {
		return private, nil
	}
	key := cfg.AgentKey()
	if key == "" {
		return nil, fmt.Errorf("upload.backend=v7 needs an Agent Key (run `wpsdav tui`, set WPSDAV_AGENT_API_KEY, or upload.agent_api_key_file)")
	}
	tokens := agent.NewTokenSource(key)
	v7 := &kvfs.V7Uploader{Client: wpsv7.New(tokens), Lister: client}
	if cfg.Upload.Fallback {
		logging.Infof("upload backend: v7 (fallback to private, downloads unchanged)")
		return &kvfs.FallbackUploader{Primary: v7, Secondary: private}, nil
	}
	logging.Infof("upload backend: v7 (downloads unchanged)")
	return v7, nil
}

func cmdTUI(args []string) {
	fs := pflag.NewFlagSet("tui", pflag.ExitOnError)
	path := fs.StringP("config", "c", "", "config file")
	_ = fs.Parse(os.Args[2:])
	if err := tui.Run(context.Background(), tui.Options{ConfigPath: *path, Version: version}); err != nil {
		logging.Fatalf("tui: %v", err)
	}
}

func cmdService(args []string) {
	if len(args) == 0 {
		logging.Fatalf("usage: wpsdav service enable|disable|status|restart")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "enable":
		if configPath("") == "" {
			logging.Warnf("no config found; the service will fail to start (run `wpsdav tui` first)")
		}
		if err := service.Enable(ctx); err != nil {
			logging.Fatalf("%v", err)
		}
		fmt.Println(ui.Green("autostart enabled"))
	case "disable":
		if err := service.Disable(ctx); err != nil {
			logging.Fatalf("%v", err)
		}
		fmt.Println(ui.Green("autostart disabled"))
	case "restart":
		if err := service.Restart(ctx); err != nil {
			logging.Fatalf("%v", err)
		}
		fmt.Println(ui.Green("service restarted"))
	case "status":
		st, err := service.Get(ctx)
		if err != nil {
			logging.Fatalf("%v", err)
		}
		fmt.Printf("unit: %v\nenabled: %v\nactive: %v\n", st.UnitPresent, st.Enabled, st.Active)
	default:
		logging.Fatalf("unknown service command %q (want enable|disable|status|restart)", args[0])
	}
}

func cmdServe(args []string) {
	fs := pflag.NewFlagSet("serve", pflag.ExitOnError)
	cfg := loadConfig(fs)
	if cfg.Username == "" || cfg.Password == "" {
		logging.Fatalf("username and password are required in the config (the DAV endpoint must not be open)")
	}

	client := newClient(cfg)
	prov, err := auth.New(cfg)
	if err != nil {
		logging.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, err := ensureCredentials(ctx, cfg, prov, client)
	if err != nil {
		if errors.Is(err, auth.ErrManual) || errors.Is(err, auth.ErrNoCredentials) {
			if client.Cookie() == "" {
				logging.Fatalf("no usable credentials: %v\n  run `wpsdav cookie` (manual) or `wpsdav login -provider buaa` (SSO)", err)
			}
			logging.Warnf("credentials check failed (%v); serving with the cached cookie until it expires", err)
		} else {
			logging.Fatalf("authentication failed: %v", err)
		}
	} else {
		logging.Infof("logged in: userid=%d company=%d", st.UserID, st.CurrentCompanyID)
	}

	rootID := cfg.RootGroupID
	if rootID == 0 {
		if cfg.Mode == config.ModePersonal {
			rootID, err = client.SpecialGroup(ctx)
		} else {
			err = fmt.Errorf("business mode needs root_group_id")
		}
		if err != nil {
			logging.Fatalf("resolve root group: %v", err)
		}
	}
	logging.Infof("mode=%s root_group=%d company=%d auth=%s", cfg.Mode, rootID, client.CompanyID(), prov.Name())

	tree := kvfs.NewTree(client, rootID, time.Duration(cfg.CacheTTLSeconds)*time.Second)
	uploader, err := buildUploader(cfg, client)
	if err != nil {
		logging.Fatalf("upload: %v", err)
	}
	srv := server.New(cfg, client, tree, uploader)

	bg, stopBG := context.WithCancel(context.Background())
	go prov.Run(bg, func(cookie string) {
		client.SetCookie(cookie)
		tree.ForcedRefresh()
	})
	go tree.Janitor(bg, 5*time.Minute)
	if prov.AutoRenew() {
		go superviseCredentials(bg, cfg, prov, client, tree)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: uploads and proxied downloads may be long-lived.
	}

	go func() {
		logging.Infof("listening on %s (dav path %s, download mode %s)", ui.Cyan(cfg.Listen), cfg.DAVPath, cfg.DownloadMode)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	stopBG()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	logging.Infof("bye")
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
		logging.Fatalf("provider %q does not support login; supported: buaa (北航统一身份认证); cookie credentials are installed with `wpsdav cookie`", mode)
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
			logging.Fatalf("read password: %v", err)
		}
		pass = string(pw)
	}
	if user == "" || pass == "" {
		logging.Fatalf("missing credentials: pass 学号 as an argument, or set sso.username/password in config, or %s/%s",
			"WPSDAV_SSO_USERNAME", "WPSDAV_SSO_PASSWORD")
	}

	prov := auth.NewBuaaProvider(user, pass, cfg.Auth.SSO.EntryURL, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cookie, err := prov.Renew(ctx)
	if err != nil {
		logging.Fatalf("login via %s failed: %v", prov.Name(), err)
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
		logging.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := ensureCredentials(ctx, cfg, prov, client); err != nil {
		logging.Fatalf("no valid credentials: %v", err)
	}
	groups, err := client.Groups(ctx)
	if err != nil {
		logging.Fatalf("groups: %v", err)
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
			logging.Fatalf("read stdin: %v", err)
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		logging.Fatalf("no cookie provided (argument or stdin)")
	}
	if err := os.WriteFile(cfg.CookieFile, []byte(value+"\n"), 0o600); err != nil {
		logging.Fatalf("write %s: %v", cfg.CookieFile, err)
	}
	fmt.Printf("cookie written to %s (%d bytes)\n", cfg.CookieFile, len(value))
}

func cmdDoctor(args []string) {
	fs := pflag.NewFlagSet("doctor", pflag.ExitOnError)
	cfg := loadConfig(fs)
	client := newClient(cfg)
	prov, err := auth.New(cfg)
	if err != nil {
		logging.Fatalf("auth: %v", err)
	}
	seedProvider(prov, client.Cookie())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Println(ui.Bold("== credentials =="))
	st, err := ensureCredentials(ctx, cfg, prov, client)
	if err != nil {
		fmt.Printf("%s no usable credentials: %v\n", ui.Red("FAIL"), err)
		os.Exit(1)
	}
	fmt.Printf("%s   provider=%s userid=%d company=%d company_account=%v\n",
		ui.Green("OK"), prov.Name(), st.UserID, st.CurrentCompanyID, st.IsCompanyAccount)

	fmt.Println(ui.Bold("== groups =="))
	groups, err := client.Groups(ctx)
	if err != nil {
		fmt.Printf("%s groups: %v\n", ui.Red("FAIL"), err)
	} else {
		for _, g := range groups {
			fmt.Printf("     %-16d %s\n", g.GroupID, g.Name)
		}
	}

	fmt.Println(ui.Bold("== upload =="))
	fmt.Printf("     backend=%s (download stays on the private path)\n", cfg.Upload.Backend)
	if cfg.Upload.Backend == config.UploadV7 {
		key := cfg.AgentKey()
		if key == "" {
			fmt.Printf("%s   no Agent Key (run `wpsdav tui`)\n", ui.Red("FAIL"))
		} else {
			ts := agent.NewTokenSource(key)
			tctx, tcancel := context.WithTimeout(ctx, 20*time.Second)
			_, terr := ts.Token(tctx)
			tcancel()
			if terr != nil {
				fmt.Printf("%s   Agent Key: %v\n", ui.Red("FAIL"), terr)
			} else {
				fmt.Printf("%s   Agent Key valid (token 12h, auto-renewed)\n", ui.Green("OK"))
			}
		}
		fmt.Printf("     fallback=%v\n", cfg.Upload.Fallback)
	}

	rootID := cfg.RootGroupID
	if rootID == 0 && cfg.Mode == config.ModePersonal {
		rootID, err = client.SpecialGroup(ctx)
		if err != nil {
			fmt.Printf("%s special group: %v\n", ui.Red("FAIL"), err)
		}
	}
	if rootID == 0 {
		fmt.Println(ui.Yellow("SKIP listing (no root group resolved)"))
		return
	}

	fmt.Println(ui.Bold("== listing =="))
	files, err := client.ListAll(ctx, rootID, 0)
	if err != nil {
		fmt.Printf("%s list root: %v\n", ui.Red("FAIL"), err)
		return
	}
	fmt.Printf("%s   %d entries at root\n", ui.Green("OK"), len(files))
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
		fmt.Println(ui.Yellow("SKIP download-URL test (no file at root)"))
		return
	}

	fmt.Println(ui.Bold("== download url =="))
	link, hdr, err := client.ResolveDownloadURL(ctx, sample.GroupID, sample.ID)
	if err != nil {
		fmt.Printf("%s get download url: %v\n", ui.Red("FAIL"), err)
		return
	}
	fmt.Printf("%s   link for %q\n", ui.Green("OK"), sample.Name)

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
			mark = ui.Green("OK")
			if strings.Contains(p.name, "rclone-like") {
				redirectOK = true
			}
		}
		fmt.Printf("%-4s %-28s %-40s (%s)\n", mark, p.name, state, p.note)
	}
	if redirectOK {
		fmt.Println("\n" + ui.Bold(ui.Green("recommendation: download_mode=redirect — bulk traffic bypasses wpsdav")))
	} else {
		fmt.Println("\n" + ui.Bold(ui.Yellow("recommendation: download_mode=proxy — the CDN rejects non-browser clients")))
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
