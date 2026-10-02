// Command server runs the domain scanner service: API, scheduler, notifier and log hub.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"domain_scanner/internal/appsettings"
	"domain_scanner/internal/auth"
	"domain_scanner/internal/cloudflare"
	"domain_scanner/internal/egress"
	"domain_scanner/internal/housekeeping"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/proxy"
	"domain_scanner/internal/register"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/server"
	"domain_scanner/internal/store"
	"domain_scanner/internal/verify"
	"domain_scanner/internal/wordlists"
)

const (
	sessionTTL        = 7 * 24 * time.Hour
	maxWordlistBytes  = 20 << 20
	housekeepingEvery = 15 * time.Minute
)

func main() {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "致命错误:", err)
		os.Exit(1)
	}
}

func run(cfg *Config) error {
	st, err := store.Open(filepath.Join(cfg.DataDir, "scanner.db"))
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer st.Close()
	ctx := context.Background()

	secret, err := loadSecret(ctx, st)
	if err != nil {
		return err
	}
	authn, err := auth.New(cfg.AdminPassword, secret, sessionTTL, time.Now)
	if err != nil {
		return err
	}

	// Logs go to the DB (the log window and later analysis) and, from stdoutLevel up, to stdout
	// (`docker compose logs`).
	stdoutRank := store.LevelRank(cfg.LogStdoutLevel)
	bus := logbus.New(func(es []store.LogEntry) {
		for _, e := range es {
			if store.LevelRank(e.Level) >= stdoutRank {
				fmt.Printf("%s %-5s %-9s [job %d] %s\n", e.Time.Format("2006-01-02 15:04:05"), e.Level, e.Component, e.JobID, e.Message)
			}
		}
		_ = st.InsertLogs(context.Background(), es)
	}, 2000)
	defer bus.Close()
	if max, err := st.MaxLogID(ctx); err == nil {
		bus.SetStartID(max)
	}
	if lv, ok, _ := st.GetSetting(ctx, server.KeyLogLevel); ok && lv != "" {
		bus.SetMinLevel(lv)
	}
	sys := bus.Logger("system")
	keeper := &housekeeping.Keeper{St: st, Bus: bus, Log: bus.Logger("housekeeping"), Policy: cfg.Storage,
		DBPath: filepath.Join(cfg.DataDir, "scanner.db"), Disk: housekeeping.StatfsDisk,
		UserLogLevel: func() string { return appsettings.LogLevel(st) }}

	envTG := notifier.Config{Token: cfg.TelegramToken, ChatID: cfg.TelegramChatID}
	envCF := appsettings.Cloudflare{AccountID: cfg.CFAccountID, Token: cfg.CFToken}
	// secrets known at startup must never be printed, whatever component logs them
	bus.SetSecrets(envTG.Token, envCF.Token, cfg.AdminPassword)

	// Telegram: pushes plus the long-poll for register-button presses. The update offset is
	// persisted so a press handled before a restart is never replayed.
	const offsetKey = "telegram_update_offset"
	nf := notifier.New(server.TelegramConfigFunc(st, envTG), bus, notifier.Options{
		LoadOffset: func() int64 {
			v, _, _ := st.GetSetting(context.Background(), offsetKey)
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		},
		SaveOffset: func(n int64) { _ = st.SetSetting(context.Background(), offsetKey, strconv.FormatInt(n, 10)) },
	})
	rootCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	nf.Start(rootCtx)

	// Cloudflare registrar: the final check before announcing, and the registration behind the button.
	policy := appsettings.RegisterPolicyFunc(st)
	cf := cloudflare.New(appsettings.CloudflareFunc(st, envCF))
	verifier := &verify.Verifier{St: st, CF: cf, Sink: nf, Policy: policy, Log: bus.Logger("cloudflare")}
	verifier.Start(rootCtx)
	registrar := &register.Service{St: st, CF: cf, Policy: policy, Log: bus.Logger("register")}
	nf.StartCallbacks(rootCtx, registrar)

	words := wordlists.NewManager(cfg.WordlistDir, filepath.Join(cfg.DataDir, "wordlists"))
	reg := egress.NewRegistry()
	checkers := newCheckerPool(reg, cfg.RDAPServers, func(format string, args ...any) { bus.Log("warn", 0, format, args...) })
	sched := scheduler.New(st, bus, verifier, checkers, words,
		scheduler.Options{MaxParallelJobs: cfg.MaxParallelJobs, Registry: reg})
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("恢复任务: %w", err)
	}

	mgr := proxy.NewManager(cfg.XrayBin, filepath.Join(cfg.DataDir, "xray"), bus.Logger("xray"))
	psvc := &proxy.Service{
		St: st, Mgr: mgr, Reg: reg, Log: bus.Logger("proxy"),
		TestURL: func() string {
			v, _, _ := st.GetSetting(context.Background(), server.KeyProxyTestURL)
			return v
		},
	}
	if n := mgr.CleanTemp(time.Hour); n > 0 {
		sys.Info("temp_cleaned", 0, fmt.Sprintf("清理了 %d 个遗留的代理测试配置", n), nil)
	}
	go psvc.Run(rootCtx)

	handler := server.New(server.Deps{
		Keeper: keeper, Store: st, Bus: bus, Sched: sched, Words: words, Telegram: nf, Auth: authn, Proxy: psvc, Cloudflare: cf,
		TelegramEnv: envTG, CloudflareEnv: envCF, MaxWordlistBytes: maxWordlistBytes, TrustProxy: cfg.TrustProxy,
	})
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	// Retention, size cap, disk guard: what keeps a long-running instance from filling its disk.
	keeper.Dir = cfg.DataDir
	go keeper.Loop(rootCtx, housekeepingEvery)

	sys.Info("start", 0, fmt.Sprintf("服务启动,监听 %s(口令保护已启用;Telegram %s;xray %s)", cfg.ListenAddr,
		tgState(server.TelegramConfigFunc(st, envTG)()), xrayState(mgr)),
		logbus.Fields{"listen": cfg.ListenAddr, "log_level": bus.MinLevel(), "stdout_level": cfg.LogStdoutLevel,
			"max_parallel_proxy_jobs": cfg.MaxParallelJobs, "trust_proxy": cfg.TrustProxy, "xray_bin": cfg.XrayBin})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-rootCtx.Done():
	}

	sys.Info("stop", 0, "收到停止信号,正在保存进度并退出…", nil)
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	sched.Shutdown(shutCtx)
	mgr.Stop()
	verifier.Stop() // flush hits still waiting for their Cloudflare check
	nf.Stop()
	return nil
}

func tgState(c notifier.Config) string {
	if c.Token != "" && c.ChatID != "" {
		return "已配置"
	}
	return "未配置"
}

func xrayState(m *proxy.Manager) string {
	if m.Available() {
		return "可用"
	}
	return "未找到二进制"
}

// loadSecret returns the persisted session secret, creating it on first start.
func loadSecret(ctx context.Context, st *store.Store) ([]byte, error) {
	const key = "session_secret"
	if v, ok, err := st.GetSetting(ctx, key); err != nil {
		return nil, err
	} else if ok {
		if b, err := hex.DecodeString(v); err == nil && len(b) >= 16 {
			return b, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.New("无法生成会话密钥")
	}
	if err := st.SetSetting(ctx, key, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	return b, nil
}
