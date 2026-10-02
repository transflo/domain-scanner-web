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
	"syscall"
	"time"

	"domain_scanner/internal/auth"
	"domain_scanner/internal/egress"
	"domain_scanner/internal/logbus"
	"domain_scanner/internal/notifier"
	"domain_scanner/internal/proxy"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/server"
	"domain_scanner/internal/store"
	"domain_scanner/internal/wordlists"
)

const (
	sessionTTL       = 7 * 24 * time.Hour
	maxWordlistBytes = 20 << 20
	// Per-level retention: per-step debug lines are plentiful, so they are capped separately and
	// can never push warnings and errors out of the history.
	debugLogsToKeep = 200_000
	otherLogsToKeep = 500_000
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

	envTG := notifier.Config{Token: cfg.TelegramToken, ChatID: cfg.TelegramChatID}
	nf := notifier.New(server.TelegramConfigFunc(st, envTG), bus, notifier.Options{})
	rootCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	nf.Start(rootCtx)

	words := wordlists.NewManager(cfg.WordlistDir, filepath.Join(cfg.DataDir, "wordlists"))
	reg := egress.NewRegistry()
	checkers := newCheckerPool(reg, cfg.RDAPServers, func(format string, args ...any) { bus.Log("warn", 0, format, args...) })
	sched := scheduler.New(st, bus, nf, checkers, words,
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
	go psvc.Run(rootCtx)

	handler := server.New(server.Deps{
		Store: st, Bus: bus, Sched: sched, Words: words, Telegram: nf, Auth: authn, Proxy: psvc,
		TelegramEnv: envTG, MaxWordlistBytes: maxWordlistBytes, TrustProxy: cfg.TrustProxy,
	})
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() { // keep the log table bounded
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				_ = st.PruneLogsByLevel(context.Background(), debugLogsToKeep, otherLogsToKeep)
			}
		}
	}()

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
