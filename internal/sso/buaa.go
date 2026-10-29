// Package sso implements the optional BUAA unified login (CAS) → WPS 365
// cookie flow used by wps.buaa.edu.cn. It is a Go port of the protocol
// documented and verified in comate-gateway (docs/sso-login.md, sso.py).
package sso

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultEntryURL redirects into the BUAA CAS login page.
	DefaultEntryURL = "https://wps.buaa.edu.cn/"
	// WPSAppID is the BUAA enterprise app on open.wps.cn (official constant).
	WPSAppID      = "AK20211203RLXZYH"
	accountBase   = "https://account.wps.cn"
	defaultUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	sessionCookie = "wps_sid"
)

var (
	executionRe = regexp.MustCompile(`name="execution" value="([^"]+)"`)
	errorTextRe = []*regexp.Regexp{
		regexp.MustCompile(`class="[^"]*(?:errors?|alert-danger|login-tips)[^"]*"[^>]*>\s*([^<]{2,120})`),
		regexp.MustCompile(`id="(?:msg|loginErrorMsg)"[^>]*>\s*([^<]{2,120})`),
	}
	captchaMarkers = []string{"请输入验证码", `name="captcha"`, "captchaRequired", "verifycode"}
)

// Error carries a machine-readable failure kind.
type Error struct {
	Kind    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Session is the result of one successful login.
type Session struct {
	SID         string
	Cookies     map[string]string
	UID         int64
	CompanyID   int64
	Nickname    string
	CompanyName string
	ExpiresIn   int
	ObtainedAt  time.Time
}

// CookieHeader renders the collected cookies as a Cookie header value.
func (s *Session) CookieHeader() string {
	names := make([]string, 0, len(s.Cookies))
	for k := range s.Cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+s.Cookies[k])
	}
	return strings.Join(parts, "; ")
}

// cookieRec remembers which domain a cookie belongs to so the final header
// only carries .wps.cn / .kdocs.cn cookies (not CAS gateway session cookies).
type cookieRec struct {
	value  string
	domain string
}

// Client runs one login/refresh flow with its own cookie jar.
type Client struct {
	http     *http.Client
	jar      http.CookieJar
	ua       string
	entryURL string
	mu       sync.Mutex
	cookies  map[string]cookieRec
}

// New creates a client. entryURL may be empty for the default.
func New(entryURL, ua string) *Client {
	if entryURL == "" {
		entryURL = DefaultEntryURL
	}
	if ua == "" {
		ua = defaultUA
	}
	jar, _ := cookiejar.New(nil)
	return &Client{
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		jar:      jar,
		ua:       ua,
		entryURL: entryURL,
		cookies:  make(map[string]cookieRec),
	}
}

func (c *Client) collect(resp *http.Response) {
	host := ""
	if resp.Request != nil && resp.Request.URL != nil {
		host = resp.Request.URL.Hostname()
	}
	for _, ck := range resp.Cookies() {
		domain := strings.TrimPrefix(ck.Domain, ".")
		if domain == "" {
			domain = host
		}
		if !isWPSDomain(domain) {
			continue
		}
		c.mu.Lock()
		c.cookies[ck.Name] = cookieRec{value: ck.Value, domain: domain}
		c.mu.Unlock()
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Kind: "network", Message: "网络请求失败：" + err.Error()}
	}
	c.collect(resp)
	return resp, nil
}

// follow GETs url and follows redirects until a non-3xx response.
func (c *Client) follow(ctx context.Context, rawURL string) (*http.Response, string, error) {
	current := rawURL
	for hop := 0; hop < 15; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, "", &Error{Kind: "unexpected", Message: err.Error()}
		}
		resp, err := c.do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if loc == "" {
				return nil, "", &Error{Kind: "unexpected", Message: "重定向缺少 Location"}
			}
			next, err := resp.Request.URL.Parse(loc)
			if err != nil {
				next, err = url.Parse(loc)
				if err != nil {
					return nil, "", &Error{Kind: "unexpected", Message: "无法解析重定向地址：" + loc}
				}
			}
			current = next.String()
			continue
		}
		return resp, current, nil
	}
	return nil, "", &Error{Kind: "unexpected", Message: "重定向次数过多"}
}

// Login runs the full CAS → ssid → ECDSA login chain.
func (c *Client) Login(ctx context.Context, username, password string) (*Session, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, &Error{Kind: "bad_request", Message: "学号/密码不能为空"}
	}

	// 1. entry → CAS login page
	page, pageURL, err := c.follow(ctx, c.entryURL)
	if err != nil {
		return nil, err
	}
	html, err := io.ReadAll(io.LimitReader(page.Body, 4<<20))
	page.Body.Close()
	if err != nil {
		return nil, &Error{Kind: "network", Message: "读取登录页失败：" + err.Error()}
	}
	if !strings.Contains(string(html), `id="loginForm"`) {
		return nil, &Error{Kind: "unexpected", Message: "没有到达统一身份认证登录页：页面结构可能变了"}
	}
	m := executionRe.FindSubmatch(html)
	if m == nil {
		return nil, &Error{Kind: "unexpected", Message: "登录页里找不到 execution 字段"}
	}
	execution := string(m[1])

	// 2. submit credentials
	form := url.Values{
		"username":  {username},
		"password":  {password},
		"type":      {"username_password"},
		"execution": {execution},
		"_eventId":  {"submit"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pageURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &Error{Kind: "unexpected", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		text := string(body)
		for _, marker := range captchaMarkers {
			if strings.Contains(text, marker) {
				return nil, &Error{Kind: "captcha", Message: "统一身份认证要求输入验证码：请先在浏览器里手动登录一次 https://sso.buaa.edu.cn 解除"}
			}
		}
		detail := extractError(text)
		if detail == "" {
			detail = "用户名或密码错误"
		}
		return nil, &Error{Kind: "bad_credentials", Message: "统一身份认证拒绝了登录：" + detail}
	case resp.StatusCode < 300 || resp.StatusCode >= 400:
		return nil, &Error{Kind: "unexpected", Message: fmt.Sprintf("登录提交返回了 HTTP %d（期望 302）", resp.StatusCode)}
	}

	// 3. follow ticket → ssoafterlogin
	loc := resp.Header.Get("Location")
	ticketURL, err := resp.Request.URL.Parse(loc)
	if err != nil {
		return nil, &Error{Kind: "unexpected", Message: "无法解析 ticket 重定向"}
	}
	after, afterURL, err := c.follow(ctx, ticketURL.String())
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(after.Body, 1<<20))
	after.Body.Close()
	if !strings.Contains(afterURL, "ssoafterlogin") {
		return nil, &Error{Kind: "unexpected", Message: "ticket 链路没有落到 ssoafterlogin：WPS 侧流程可能变了"}
	}
	parsed, err := url.Parse(afterURL)
	if err != nil {
		return nil, &Error{Kind: "unexpected", Message: err.Error()}
	}
	query := parsed.Query()

	// 4. cas code → ssid
	ssid, err := c.verifyActive(ctx, query, afterURL)
	if err != nil {
		return nil, err
	}

	// 5. account list
	uid, companyID, nickname, companyName, err := c.fetchUsers(ctx, ssid, afterURL)
	if err != nil {
		return nil, err
	}

	// 6. ECDSA-signed login → Set-Cookie: wps_sid ...
	if err := c.secureLogin(ctx, ssid, []int64{uid}, afterURL); err != nil {
		return nil, err
	}

	c.mu.Lock()
	cookies := make(map[string]string, len(c.cookies))
	for k, v := range c.cookies {
		cookies[k] = v.value
	}
	c.mu.Unlock()
	sid := cookies[sessionCookie]
	if sid == "" {
		return nil, &Error{Kind: "unexpected", Message: "登录成功但没有拿到 wps_sid"}
	}
	expiresIn := 0
	if v := cookies["exp"]; v != "" {
		fmt.Sscanf(v, "%d", &expiresIn)
	}
	return &Session{
		SID:         sid,
		Cookies:     cookies,
		UID:         uid,
		CompanyID:   companyID,
		Nickname:    nickname,
		CompanyName: companyName,
		ExpiresIn:   expiresIn,
		ObtainedAt:  time.Now(),
	}, nil
}

func (c *Client) verifyActive(ctx context.Context, query url.Values, referer string) (string, error) {
	csrf := randomToken(32)
	if u, err := url.Parse(accountBase + "/"); err == nil {
		c.jar.SetCookies(u, []*http.Cookie{{Name: "csrf", Value: csrf, Path: "/", Domain: "wps.cn"}})
	}
	c.mu.Lock()
	c.cookies["csrf"] = cookieRec{value: csrf, domain: "wps.cn"}
	c.mu.Unlock()

	form := url.Values{
		"action":              {valueOr(query, "action", "verify")},
		"app_id":              {valueOr(query, "app_id", WPSAppID)},
		"cb":                  {query.Get("cb")},
		"code":                {query.Get("code")},
		"login_reg":           {valueOr(query, "login_reg", "cn")},
		"oauth_state":         {query.Get("oauth_state")},
		"state":               {query.Get("state")},
		"utype":               {valueOr(query, "utype", "plus")},
		"csrfmiddlewaretoken": {csrf},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountBase+"/api/v4/oauth/plus/verify_active", strings.NewReader(form.Encode()))
	if err != nil {
		return "", &Error{Kind: "unexpected", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", referer)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var data struct {
		Result string `json:"result"`
		SSID   string `json:"ssid"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", &Error{Kind: "unexpected", Message: fmt.Sprintf("verify_active 返回 HTTP %d 且不是 JSON", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK || data.Result != "ok" || data.SSID == "" {
		return "", &Error{Kind: "upstream", Message: fmt.Sprintf("verify_active 失败（HTTP %d）：%.200s", resp.StatusCode, string(body))}
	}
	return data.SSID, nil
}

func (c *Client) fetchUsers(ctx context.Context, ssid, referer string) (uid, companyID int64, nickname, companyName string, err error) {
	u := fmt.Sprintf("%s/api/v3/login/users?ssid=%s&filter_rule=normal&check_rule=second_phone", accountBase, url.QueryEscape(ssid))
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if rerr != nil {
		return 0, 0, "", "", &Error{Kind: "unexpected", Message: rerr.Error()}
	}
	req.Header.Set("Referer", referer)
	resp, rerr := c.do(req)
	if rerr != nil {
		return 0, 0, "", "", rerr
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var data struct {
		Users []struct {
			UserID      int64  `json:"userid"`
			Nickname    string `json:"nickname"`
			CompanyID   int64  `json:"company_id"`
			CompanyName string `json:"company_name"`
			Default     bool   `json:"default_select"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, 0, "", "", &Error{Kind: "unexpected", Message: "login/users 返回的不是 JSON"}
	}
	if len(data.Users) == 0 {
		return 0, 0, "", "", &Error{Kind: "unexpected", Message: "login/users 没有返回账号"}
	}
	sort.SliceStable(data.Users, func(i, j int) bool { return data.Users[i].Default && !data.Users[j].Default })
	u0 := data.Users[0]
	if u0.UserID == 0 {
		return 0, 0, "", "", &Error{Kind: "unexpected", Message: "login/users 的 users[] 里没有 userid"}
	}
	return u0.UserID, u0.CompanyID, u0.Nickname, u0.CompanyName, nil
}

// signSSID creates the ephemeral P-256 key and the login signature:
// public_key = base64url(compact JWK), ssid_sign = base64url(r||s).
func signSSID(ssid string) (*ecdsa.PrivateKey, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", &Error{Kind: "unexpected", Message: "生成签名密钥失败：" + err.Error()}
	}
	jwk := map[string]any{
		"crv":     "P-256",
		"ext":     true,
		"key_ops": []string{"verify"},
		"kty":     "EC",
		"x":       b64u(pad32(key.PublicKey.X)),
		"y":       b64u(pad32(key.PublicKey.Y)),
	}
	jwkJSON, _ := json.Marshal(jwk) // map keys marshal in sorted order (matches the SDK)
	hash := sha256.Sum256([]byte(ssid))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return nil, "", "", &Error{Kind: "unexpected", Message: "签名失败：" + err.Error()}
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return key, b64u(jwkJSON), b64u(sig), nil
}

func (c *Client) secureLogin(ctx context.Context, ssid string, userIDs []int64, referer string) error {
	_, publicKey, ssidSign, err := signSSID(ssid)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"user_ids":    userIDs,
		"ssid":        ssid,
		"page":        "login",
		"append_only": false,
		"slv":         "ecdsa_itk",
		"public_key":  publicKey,
		"ssid_sign":   ssidSign,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountBase+"/passport/secure/api/login", strings.NewReader(string(body)))
	if err != nil {
		return &Error{Kind: "unexpected", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", referer)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var result struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(respBody, &result)
	if resp.StatusCode != http.StatusOK || result.Result != "ok" {
		return &Error{Kind: "upstream", Message: fmt.Sprintf("secure/api/login 失败（HTTP %d）：%.200s", resp.StatusCode, string(respBody))}
	}
	return nil
}

// Refresh renews a session with grant_token (rtk cookie). On success the
// returned session carries the possibly-updated cookies.
func (c *Client) Refresh(ctx context.Context, cookies map[string]string) (*Session, bool) {
	if u, err := url.Parse(accountBase + "/"); err == nil {
		cookiesList := make([]*http.Cookie, 0, len(cookies))
		for k, v := range cookies {
			cookiesList = append(cookiesList, &http.Cookie{Name: k, Value: v, Path: "/", Domain: "wps.cn"})
		}
		c.jar.SetCookies(u, cookiesList)
	}
	c.mu.Lock()
	for k, v := range cookies {
		c.cookies[k] = cookieRec{value: v, domain: "wps.cn"}
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountBase+"/passport/secure/api/grant_token",
		strings.NewReader(`{"grant_type":"refresh_token"}`))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", accountBase+"/permit/ssoafterlogin.html")
	resp, err := c.do(req)
	if err != nil {
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var data struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &data) != nil {
		return nil, false
	}
	if resp.StatusCode != http.StatusOK || (data.Result != "ok" && data.Result != "TokenExpired") {
		return nil, false
	}
	c.mu.Lock()
	merged := make(map[string]string, len(c.cookies))
	for k, v := range c.cookies {
		merged[k] = v.value
	}
	c.mu.Unlock()
	sid := merged[sessionCookie]
	if sid == "" {
		return nil, false
	}
	expiresIn := 0
	if v := merged["exp"]; v != "" {
		fmt.Sscanf(v, "%d", &expiresIn)
	}
	return &Session{
		SID:        sid,
		Cookies:    merged,
		ExpiresIn:  expiresIn,
		ObtainedAt: time.Now(),
	}, true
}

// Manager serializes login/refresh attempts for one account.
type Manager struct {
	Username string
	Password string
	EntryURL string

	mu       sync.Mutex
	session  *Session
	lastFail time.Time
}

// NewManager builds a manager; password may come from env at the call site.
func NewManager(username, password, entryURL string) *Manager {
	return &Manager{Username: username, Password: password, EntryURL: entryURL}
}

// CookieHeader returns the current cookie header value (empty before login).
func (m *Manager) CookieHeader() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return ""
	}
	return m.session.CookieHeader()
}

// Login performs a full login and stores the session.
func (m *Manager) Login(ctx context.Context) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client := New(m.EntryURL, "")
	sess, err := client.Login(ctx, m.Username, m.Password)
	if err != nil {
		m.lastFail = time.Now()
		return nil, err
	}
	m.session = sess
	return sess, nil
}

// Refresh renews the session: grant_token first, full login as fallback.
func (m *Manager) Refresh(ctx context.Context) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		client := New(m.EntryURL, "")
		if refreshed, ok := client.Refresh(ctx, m.session.Cookies); ok {
			if refreshed.UID == 0 {
				refreshed.UID = m.session.UID
				refreshed.CompanyID = m.session.CompanyID
				refreshed.Nickname = m.session.Nickname
				refreshed.CompanyName = m.session.CompanyName
			}
			m.session = refreshed
			return refreshed, nil
		}
	}
	if m.Username == "" || m.Password == "" {
		return nil, &Error{Kind: "no_credentials", Message: "会话已失效，且没有配置用于重新登录的账号密码"}
	}
	client := New(m.EntryURL, "")
	sess, err := client.Login(ctx, m.Username, m.Password)
	if err != nil {
		m.lastFail = time.Now()
		return nil, err
	}
	m.session = sess
	return sess, nil
}

// Seed installs a previously obtained cookie header as the current session.
// This lets a process that only has a cached cookie (no password) renew the
// session with grant_token while the rtk cookie is still valid.
func (m *Manager) Seed(header string) bool {
	cookies := ParseCookieHeader(header)
	sid := cookies[sessionCookie]
	if sid == "" {
		return false
	}
	expiresIn := 0
	if v := cookies["exp"]; v != "" {
		fmt.Sscanf(v, "%d", &expiresIn)
	}
	m.mu.Lock()
	m.session = &Session{
		SID:        sid,
		Cookies:    cookies,
		ExpiresIn:  expiresIn,
		ObtainedAt: time.Now(),
	}
	m.mu.Unlock()
	return true
}

// ParseCookieHeader turns "a=1; b=2" into a map.
func ParseCookieHeader(header string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// Session returns the cached session (nil before login).
func (m *Manager) Session() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session
}

func pad32(n *big.Int) []byte {
	out := make([]byte, 32)
	n.FillBytes(out)
	return out
}

func b64u(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func randomToken(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	for i := range buf {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			buf[i] = alphabet[0]
			continue
		}
		buf[i] = alphabet[idx.Int64()]
	}
	return string(buf)
}

func valueOr(q url.Values, key, def string) string {
	if v := q.Get(key); v != "" {
		return v
	}
	return def
}

// isWPSDomain reports whether cookies for domain are relevant to the WPS
// APIs (and not, say, a CAS gateway session cookie).
func isWPSDomain(domain string) bool {
	switch {
	case domain == "wps.cn", strings.HasSuffix(domain, ".wps.cn"):
		return true
	case domain == "kdocs.cn", strings.HasSuffix(domain, ".kdocs.cn"):
		return true
	}
	return false
}

func extractError(html string) string {
	for _, re := range errorTextRe {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			if text := strings.TrimSpace(m[1]); text != "" {
				return text
			}
		}
	}
	return ""
}
