// Package tui provides an interactive setup and operations menu.
package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"wpsdav/internal/agent"
	"wpsdav/internal/config"
	"wpsdav/internal/service"
	"wpsdav/internal/setup"
	"wpsdav/internal/ui"
	"wpsdav/internal/wps"
)

// Options configures the menu.
type Options struct {
	ConfigPath string
	Version    string
}

type app struct {
	opts Options
	in   *bufio.Reader
	path string
	has  bool
}

// Run starts the interactive menu. It requires a terminal.
func Run(ctx context.Context, opts Options) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("wpsdav tui requires an interactive terminal")
	}
	path, ok := resolveConfig(opts.ConfigPath)
	a := &app{opts: opts, in: bufio.NewReader(os.Stdin), path: path, has: ok}
	for {
		a.clear()
		a.header()
		choice := a.ask("选择操作: ")
		switch strings.TrimSpace(choice) {
		case "1":
			a.configMenu(ctx)
		case "2":
			a.loginMenu(ctx)
		case "3":
			a.uploadMenu(ctx)
		case "4":
			a.serviceMenu(ctx)
		case "5":
			a.diagnose(ctx)
		case "0", "q", "Q":
			return nil
		case "":
		default:
			a.pause("无效选项，按回车继续")
		}
	}
}

func resolveConfig(explicit string) (string, bool) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, true
		}
		return explicit, false
	}
	for _, p := range config.SearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return setup.UserConfigPath(), false
}

func (a *app) load() *config.Config {
	if !a.has {
		return nil
	}
	cfg, err := config.Load(a.path)
	if err != nil {
		a.pause("配置读取失败: " + err.Error())
		return nil
	}
	return cfg
}

func (a *app) clear() { fmt.Print("\033[2J\033[H") }

func (a *app) header() {
	fmt.Println(ui.Bold("wpsdav") + ui.Dim("  "+a.opts.Version))
	fmt.Println(ui.Dim(strings.Repeat("─", 46)))
	fmt.Printf("  配置   %s\n", a.path)
	cfg := a.load()
	if cfg == nil {
		fmt.Println("  状态   " + ui.Yellow("尚未初始化"))
	} else {
		cookie := "未设置"
		if _, err := os.Stat(cfg.CookieFile); err == nil {
			cookie = "已保存"
		}
		agentKey := "未设置"
		if cfg.HasAgentKey() {
			agentKey = "已保存"
		}
		fmt.Printf("  登录   %s（Cookie %s）\n", cfg.Auth.Mode, cookie)
		fmt.Printf("  上传   %s（Agent Key %s）\n", cfg.Upload.Backend, agentKey)
		fmt.Printf("  监听   %s\n", cfg.Listen)
	}
	if st, err := service.Get(context.Background()); err == nil {
		state := "未启用"
		if st.Enabled {
			state = "已启用"
		}
		if st.Active {
			state += "，运行中"
		}
		if !st.UnitPresent {
			state = "未安装用户单元"
		}
		fmt.Printf("  自启   %s\n", state)
	} else if err == service.ErrUnsupported {
		fmt.Println("  自启   " + ui.Dim("当前平台不支持 systemd 用户服务"))
	}
	fmt.Println()
	fmt.Println("  1  初始化 / 查看配置")
	fmt.Println("  2  登录与凭据")
	fmt.Println("  3  上传方式")
	fmt.Println("  4  开机自启")
	fmt.Println("  5  运行诊断")
	fmt.Println("  0  退出")
	fmt.Println()
}

func (a *app) ask(prompt string) string {
	fmt.Print(ui.Cyan(prompt))
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line)
}

func (a *app) askHidden(prompt string) string {
	fmt.Print(ui.Cyan(prompt))
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *app) pause(msg string) {
	if msg != "" {
		fmt.Println(msg)
	}
	fmt.Print(ui.Dim("按回车继续..."))
	_, _ = a.in.ReadString('\n')
}

func (a *app) configMenu(ctx context.Context) {
	a.clear()
	cfg := a.load()
	if cfg != nil {
		fmt.Println(ui.Bold("当前配置"))
		fmt.Printf("  路径      %s\n", cfg.SourcePath)
		fmt.Printf("  监听      %s\n", cfg.Listen)
		fmt.Printf("  WebDAV    %s / %s\n", cfg.Username, "******")
		fmt.Printf("  Cookie    %s\n", cfg.CookieFile)
		fmt.Printf("  登录方式  %s\n", cfg.Auth.Mode)
		fmt.Printf("  上传方式  %s\n", cfg.Upload.Backend)
		fmt.Printf("  Agent Key %s\n", cfg.Upload.AgentAPIKeyFile)
		fmt.Println()
		fmt.Println("  1  重新生成配置（会覆盖现有文件）")
		fmt.Println("  0  返回")
		if a.ask("选择操作: ") != "1" {
			return
		}
		if strings.ToLower(a.ask("确认覆盖？输入 yes: ")) != "yes" {
			a.pause("已取消")
			return
		}
	}
	newCfg, password, err := setup.Init(a.path, false, false, "127.0.0.1:8705", true)
	if err != nil {
		a.pause("创建失败: " + err.Error())
		return
	}
	a.has = true
	a.clear()
	fmt.Println(ui.Green("配置已创建"))
	fmt.Printf("  文件      %s\n", a.path)
	fmt.Printf("  WebDAV 用户 %s\n", newCfg.Username)
	fmt.Printf("  WebDAV 密码 %s\n", ui.Cyan(password))
	fmt.Printf("  Cookie    %s\n", newCfg.CookieFile)
	fmt.Println()
	fmt.Println("下一步：2 登录，然后 4 开启开机自启。")
	a.pause("")
}

func (a *app) loginMenu(ctx context.Context) {
	for {
		a.clear()
		fmt.Println(ui.Bold("登录与凭据"))
		fmt.Println("  1  浏览器 Cookie（最简单）")
		fmt.Println("  2  北航统一认证（自动续期）")
		fmt.Println("  3  Agent Key（v7 上传用）")
		fmt.Println("  0  返回")
		switch a.ask("选择操作: ") {
		case "1":
			a.loginCookie(ctx)
		case "2":
			a.loginBuaa(ctx)
		case "3":
			a.loginAgent(ctx)
		case "0":
			return
		}
	}
}

func (a *app) loginCookie(ctx context.Context) {
	cfg := a.load()
	if cfg == nil {
		a.pause("请先初始化配置（1）")
		return
	}
	fmt.Println("浏览器打开金山文档 → F12 → Network → 任意请求 → 复制整串 Cookie。")
	cookie := a.ask("粘贴 Cookie: ")
	if cookie == "" {
		return
	}
	if err := setup.SaveCookie(cfg, cookie); err != nil {
		a.pause("保存失败: " + err.Error())
		return
	}
	cfg.Auth.Mode = "cookie"
	if cfg.SourcePath != "" {
		_ = config.Save(cfg.SourcePath, cfg)
	}
	client := newClient(cfg)
	st, err := client.CheckLogin(ctx)
	if err != nil {
		a.pause("Cookie 已保存，但校验失败: " + err.Error())
		return
	}
	a.pause(ui.Green(fmt.Sprintf("登录成功：%s（company %d）", st.LoginMode, st.CurrentCompanyID)))
}

func (a *app) loginBuaa(ctx context.Context) {
	cfg := a.load()
	if cfg == nil {
		a.pause("请先初始化配置（1）")
		return
	}
	user := a.ask("学号: ")
	pass := a.askHidden("密码（输入不回显）: ")
	if user == "" || pass == "" {
		return
	}
	sess, err := setup.LoginBuaa(ctx, cfg, user, pass)
	if err != nil {
		a.pause("登录失败: " + err.Error())
		return
	}
	a.pause(ui.Green(fmt.Sprintf("登录成功：%s（uid %d）", sess.Nickname, sess.UID)))
}

func (a *app) loginAgent(ctx context.Context) {
	cfg := a.load()
	if cfg == nil {
		a.pause("请先初始化配置（1）")
		return
	}
	fmt.Println("在 account.wps.cn → 个人中心 → AI 服务 → 我的 AI Agent 创建并复制密钥。")
	fmt.Println("格式：apik:<id>.<secret>")
	key := a.askHidden("Agent Key（输入不回显）: ")
	if key == "" {
		return
	}
	ts := agent.NewTokenSource(key)
	if _, err := ts.Token(ctx); err != nil {
		a.pause("密钥校验失败: " + err.Error())
		return
	}
	if err := setup.SaveAgentKey(cfg, key); err != nil {
		a.pause("保存失败: " + err.Error())
		return
	}
	a.pause(ui.Green("Agent Key 已保存。需要时在 3 上传方式 中切换到 v7。"))
}

func (a *app) uploadMenu(ctx context.Context) {
	cfg := a.load()
	if cfg == nil {
		a.pause("请先初始化配置（1）")
		return
	}
	a.clear()
	fmt.Println(ui.Bold("上传方式"))
	fmt.Println("  private  使用 Cookie 私有接口（默认，兼容性最好）")
	fmt.Println("  v7       官方 API（Agent Key；大文件分块，同哈希跳过，失败回退）")
	fmt.Println()
	fmt.Printf("当前：%s\n\n", cfg.Upload.Backend)
	fmt.Println("  1  private")
	fmt.Println("  2  v7")
	fmt.Println("  0  返回")
	switch a.ask("选择: ") {
	case "1":
		cfg.Upload.Backend = config.UploadPrivate
		if err := config.Save(cfg.SourcePath, cfg); err != nil {
			a.pause("保存失败: " + err.Error())
			return
		}
		a.pause(ui.Green("已切换为 private 上传。"))
	case "2":
		if !cfg.HasAgentKey() {
			fmt.Println("还没有 Agent Key，请先输入。")
			key := a.askHidden("Agent Key（输入不回显）: ")
			if key == "" {
				return
			}
			ts := agent.NewTokenSource(key)
			if _, err := ts.Token(ctx); err != nil {
				a.pause("密钥校验失败: " + err.Error())
				return
			}
			if err := setup.SaveAgentKey(cfg, key); err != nil {
				a.pause("保存失败: " + err.Error())
				return
			}
		}
		cfg.Upload.Backend = config.UploadV7
		if err := config.Save(cfg.SourcePath, cfg); err != nil {
			a.pause("保存失败: " + err.Error())
			return
		}
		a.pause(ui.Green("已切换为 v7 上传（下载仍走原路径）。"))
	}
}

func (a *app) serviceMenu(ctx context.Context) {
	a.clear()
	st, err := service.Get(ctx)
	if err == service.ErrUnsupported {
		a.pause("当前平台不支持 systemd 用户服务。")
		return
	}
	if err != nil {
		a.pause("读取服务状态失败: " + err.Error())
		return
	}
	state := "未启用"
	if st.Enabled {
		state = "已启用"
	}
	if st.Active {
		state += "，运行中"
	}
	fmt.Println(ui.Bold("开机自启"))
	fmt.Printf("  状态: %s\n", state)
	if !st.UnitPresent {
		fmt.Println(ui.Yellow("  未找到 wpsdav.service：AUR 包自带该单元；源码安装请手动放置到 ~/.config/systemd/user/。"))
	}
	fmt.Println()
	fmt.Println("  1  启用并启动")
	fmt.Println("  2  停用并停止")
	fmt.Println("  0  返回")
	switch a.ask("选择: ") {
	case "1":
		if !a.has {
			a.pause("配置尚未初始化，服务启动会失败。请先完成 1 和 2。")
			return
		}
		if err := service.Enable(ctx); err != nil {
			a.pause("启用失败: " + err.Error())
			return
		}
		a.pause(ui.Green("已启用，并设置为登录后自动启动。"))
	case "2":
		if err := service.Disable(ctx); err != nil {
			a.pause("停用失败: " + err.Error())
			return
		}
		a.pause(ui.Green("已停用并移出开机自启。"))
	}
}

func (a *app) diagnose(ctx context.Context) {
	a.clear()
	cfg := a.load()
	if cfg == nil {
		a.pause("请先初始化配置（1）")
		return
	}
	fmt.Println(ui.Bold("诊断"))
	ok := func(name string, err error, detail string) {
		if err != nil {
			fmt.Printf("  %s %-10s %v\n", ui.Red("FAIL"), name, err)
			return
		}
		fmt.Printf("  %s %-10s %s\n", ui.Green("OK"), name, detail)
	}
	if _, err := os.Stat(cfg.CookieFile); err != nil {
		ok("Cookie", err, "")
	} else {
		client := newClient(cfg)
		st, err := client.CheckLogin(ctx)
		detail := ""
		if err == nil {
			detail = fmt.Sprintf("user %d / company %d", st.UserID, st.CurrentCompanyID)
		}
		ok("Cookie", err, detail)
	}
	if cfg.Upload.Backend == config.UploadV7 {
		ts := agent.NewTokenSource(cfg.AgentKey())
		_, err := ts.Token(ctx)
		ok("Agent Key", err, "v7 上传可用")
	} else {
		fmt.Printf("  %s %-10s 使用私有上传\n", ui.Dim("--"), "上传")
	}
	st, err := service.Get(ctx)
	switch {
	case err == service.ErrUnsupported:
		fmt.Printf("  %s %-10s 当前平台不支持\n", ui.Dim("--"), "自启")
	case err != nil:
		ok("自启", err, "")
	default:
		detail := "未启用"
		if st.Enabled {
			detail = "已启用"
			if st.Active {
				detail += "，运行中"
			}
		}
		ok("自启", nil, detail)
	}
	a.pause("")
}

func newClient(cfg *config.Config) *wps.Client {
	client := wps.New(cfg)
	if data, err := os.ReadFile(cfg.CookieFile); err == nil {
		client.SetCookie(strings.TrimSpace(string(data)))
	}
	return client
}
