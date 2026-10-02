package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"domain_scanner/internal/logbus"
)

// Process is a running xray instance.
type Process interface {
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// Stop terminates the process; it is safe to call more than once.
	Stop()
}

// Starter launches xray with a configuration file. logf receives every output line of the
// process with a level guessed from Xray's "[Warning]"-style prefix. Replaceable for tests.
type Starter func(ctx context.Context, bin, cfgPath string, logf func(level, line string)) (Process, error)

const (
	readyTimeout    = 8 * time.Second
	maxRestartDelay = 30 * time.Second
)

// Manager keeps one long-running xray process whose configuration exposes every enabled
// outbound as a loopback SOCKS5 listener, restarts it when the set of outbounds changes, and
// revives it when it crashes.
type Manager struct {
	Bin      string
	Dir      string
	BasePort int
	Starter  Starter
	// RestartBackoff is the first wait before reviving a crashed process (doubles up to 30s).
	RestartBackoff time.Duration
	// SkipBinaryCheck lets tests run with a fake Starter and no xray binary on disk.
	SkipBinaryCheck bool

	lg      *logbus.Logger
	testSem chan struct{}

	mu      sync.Mutex
	gen     int
	proc    Process
	cancel  context.CancelFunc
	ports   map[int64]int
	lastErr string
}

func NewManager(bin, dir string, lg *logbus.Logger) *Manager {
	return &Manager{Bin: bin, Dir: dir, BasePort: BasePort, Starter: execStart, RestartBackoff: time.Second,
		lg: lg, testSem: make(chan struct{}, 2), ports: map[int64]int{}}
}

// Available reports whether the xray binary exists.
func (m *Manager) Available() bool {
	if m.SkipBinaryCheck {
		return true
	}
	st, err := os.Stat(m.Bin)
	return err == nil && !st.IsDir()
}

func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc != nil
}

// LastError explains why xray is not (or no longer) running; empty when healthy.
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Port returns the local SOCKS5 port serving outbound id.
func (m *Manager) Port(id int64) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.ports[id]
	return p, ok
}

func (m *Manager) logf(level, line string) {
	lv := level
	if strings.Contains(line, "[Error]") {
		lv = "error"
	} else if strings.Contains(line, "[Warning]") {
		lv = "warn"
	} else if strings.Contains(line, "[Info]") || strings.Contains(line, "[Debug]") {
		lv = "debug"
	}
	m.lg.Emit(lv, "output", 0, strings.TrimSpace(line), nil)
}

// Apply makes xray serve exactly the given outbounds. With no entries xray is stopped. The
// returned map is outbound id -> local SOCKS5 port. An invalid configuration is rejected before
// the running instance is touched.
func (m *Manager) Apply(ctx context.Context, entries []Entry) (map[int64]int, error) {
	var raw []byte
	var ports map[int64]int
	if len(entries) > 0 {
		var err error
		if raw, ports, err = BuildConfig(entries, m.BasePort); err != nil {
			m.setErr("配置无效:" + err.Error())
			return nil, err
		}
	}

	m.mu.Lock()
	m.stopLocked()
	if len(entries) == 0 {
		m.ports, m.lastErr = map[int64]int{}, ""
		m.mu.Unlock()
		m.lg.Info("stopped", 0, "没有启用的出站代理,xray 未运行", nil)
		return map[int64]int{}, nil
	}
	gen := m.gen
	m.mu.Unlock()

	if !m.Available() {
		err := fmt.Errorf("未找到 xray 可执行文件 %s(镜像内应已内置;本地运行请设置 XRAY_BIN)", m.Bin)
		m.setErr(err.Error())
		return nil, err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		m.setErr(err.Error())
		return nil, err
	}
	cfgPath := filepath.Join(m.Dir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		m.setErr(err.Error())
		return nil, err
	}

	t0 := time.Now()
	pctx, cancel := context.WithCancel(context.Background())
	proc, err := m.Starter(pctx, m.Bin, cfgPath, m.logf)
	if err != nil {
		cancel()
		m.setErr("启动 xray 失败:" + err.Error())
		m.lg.Error("start_failed", 0, "启动 xray 失败:"+err.Error(), logbus.Fields{"error": err.Error()})
		return nil, err
	}
	if err := waitReady(ctx, portList(ports), readyTimeout); err != nil {
		proc.Stop()
		cancel()
		m.setErr("xray 启动后端口未就绪:" + err.Error())
		m.lg.Error("not_ready", 0, "xray 已启动但本地端口未就绪:"+err.Error(), logbus.Fields{"error": err.Error()})
		return nil, err
	}

	m.mu.Lock()
	if m.gen != gen { // someone else reconfigured meanwhile
		m.mu.Unlock()
		proc.Stop()
		cancel()
		return nil, errors.New("配置在应用过程中被更新")
	}
	m.proc, m.cancel, m.ports, m.lastErr = proc, cancel, ports, ""
	m.mu.Unlock()
	m.lg.Info("started", 0, fmt.Sprintf("xray 已启动,提供 %d 个本地 SOCKS5 出口", len(ports)),
		logbus.Fields{"outbounds": len(ports), "duration_ms": time.Since(t0), "config_bytes": len(raw)})
	go m.supervise(gen, proc, cfgPath, ports)
	return ports, nil
}

func portList(ports map[int64]int) []int {
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		out = append(out, p)
	}
	return out
}

func (m *Manager) setErr(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

// supervise revives xray when it exits on its own.
func (m *Manager) supervise(gen int, proc Process, cfgPath string, ports map[int64]int) {
	delay := m.RestartBackoff
	for {
		err := proc.Wait()
		m.mu.Lock()
		if m.gen != gen { // stopped or replaced on purpose
			m.mu.Unlock()
			return
		}
		m.proc = nil
		msg := "xray 意外退出"
		if err != nil {
			msg += ":" + err.Error()
		}
		m.lastErr = msg
		m.mu.Unlock()
		m.lg.Error("crashed", 0, fmt.Sprintf("%s,%s 后重启", msg, delay), logbus.Fields{"restart_in_s": delay.Seconds()})

		time.Sleep(delay)
		if delay *= 2; delay > maxRestartDelay {
			delay = maxRestartDelay
		}
		m.mu.Lock()
		if m.gen != gen {
			m.mu.Unlock()
			return
		}
		pctx, cancel := context.WithCancel(context.Background())
		np, err := m.Starter(pctx, m.Bin, cfgPath, m.logf)
		if err != nil {
			cancel()
			m.lastErr = "重启 xray 失败:" + err.Error()
			m.mu.Unlock()
			m.lg.Error("restart_failed", 0, m.LastError(), nil)
			// keep trying with the longer delay
			proc = failedProc{}
			continue
		}
		m.cancel, m.proc, m.lastErr = cancel, np, ""
		m.mu.Unlock()
		m.lg.Info("restarted", 0, "xray 已重启", nil)
		proc = np
		delay = m.RestartBackoff
	}
}

// failedProc makes the supervise loop retry immediately after a failed restart attempt.
type failedProc struct{}

func (failedProc) Wait() error { return errors.New("restart failed") }
func (failedProc) Stop()       {}

func (m *Manager) stopLocked() {
	m.gen++
	if m.proc != nil {
		m.proc.Stop()
		m.proc = nil
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// Stop terminates xray. A later Apply starts it again.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopLocked()
	m.ports = map[int64]int{}
	m.mu.Unlock()
}

func waitReady(ctx context.Context, ports []int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, p := range ports {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		for {
			c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
			if err == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: %v", addr, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil
}

// TestConfig tests one (possibly unsaved) outbound on a throw-away xray instance, leaving the
// main instance untouched.
func (m *Manager) TestConfig(ctx context.Context, cfg json.RawMessage, o ProbeOptions) ProbeResult {
	select {
	case m.testSem <- struct{}{}:
		defer func() { <-m.testSem }()
	case <-ctx.Done():
		return ProbeResult{Error: ctx.Err().Error()}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ProbeResult{Error: "无法分配本地端口:" + err.Error()}
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	raw, err := BuildSingle(cfg, port)
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	if !m.Available() {
		return ProbeResult{Error: "未找到 xray 可执行文件 " + m.Bin}
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return ProbeResult{Error: err.Error()}
	}
	f, err := os.CreateTemp(m.Dir, "test-*.json")
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return ProbeResult{Error: err.Error()}
	}
	f.Close()

	pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	proc, err := m.Starter(pctx, m.Bin, f.Name(), m.logf)
	if err != nil {
		return ProbeResult{Error: "启动 xray 失败:" + err.Error()}
	}
	defer proc.Stop()
	if err := waitReady(pctx, []int{port}, readyTimeout); err != nil {
		return ProbeResult{Error: "xray 未能启动(配置可能被 xray 拒绝,见日志组件 xray):" + err.Error()}
	}
	return Probe(pctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), o)
}

// ---- real process ----

type execProc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	pw     *io.PipeWriter
}

func (p *execProc) Wait() error {
	err := p.cmd.Wait()
	p.pw.Close()
	return err
}

func (p *execProc) Stop() { p.cancel() }

func execStart(ctx context.Context, bin, cfgPath string, logf func(level, line string)) (Process, error) {
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, bin, "run", "-c", cfgPath)
	cmd.WaitDelay = 3 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		pw.Close()
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); strings.TrimSpace(line) != "" {
				logf("info", line)
			}
		}
	}()
	return &execProc{cmd: cmd, cancel: cancel, pw: pw}, nil
}
