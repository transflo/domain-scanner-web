package stats

import (
	"fmt"
	"strings"
	"time"
)

// StatusRenderer 状态栏渲染器
type StatusRenderer struct {
	collector   *Collector
	stopChan    chan struct{}
	doneChan    chan struct{}
	isTTY       bool
	lastLineLen int
}

// NewStatusRenderer 创建状态渲染器
func NewStatusRenderer(collector *Collector) *StatusRenderer {
	return &StatusRenderer{
		collector: collector,
		stopChan:  make(chan struct{}),
		doneChan:  make(chan struct{}),
		isTTY:     IsTerminal(),
	}
}

// Start 启动状态栏更新
func (r *StatusRenderer) Start() {
	go r.renderLoop()
}

// Stop 停止状态栏更新
func (r *StatusRenderer) Stop() {
	close(r.stopChan)
	<-r.doneChan
	// 清除状态栏
	if r.isTTY {
		r.clearLine()
	}
}

// IsTTY 返回是否为终端环境
func (r *StatusRenderer) IsTTY() bool {
	return r.isTTY
}

// renderLoop 渲染循环
func (r *StatusRenderer) renderLoop() {
	defer close(r.doneChan)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopChan:
			return
		case <-ticker.C:
			r.render()
		}
	}
}

// render 渲染单次状态栏
func (r *StatusRenderer) render() {
	if !r.isTTY {
		return
	}

	r.renderProgress()
}

// renderProgress 渲染进度条格式
func (r *StatusRenderer) renderProgress() {
	processed := r.collector.GetProcessedCount()
	available := r.collector.GetAvailableCount()
	active := r.collector.GetActiveWorkers()
	progress := r.collector.GetProgress()
	qps := r.collector.CalculateQPS()
	eta := r.collector.CalculateETA()
	total := r.collector.GetTotalDomains()
	totalWorkers := r.collector.GetTotalWorkers()

	// 构建进度条
	barWidth := 20
	filled := int(progress / 100 * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled)

	// 格式化 ETA
	etaStr := formatDuration(eta)

	// 构建状态行
	status := fmt.Sprintf(
		"\r[%s] %.1f%% | Workers: %d/%d | QPS: %.1f | ETA: %s | Found: %d | %d/%d",
		bar,
		progress,
		active,
		totalWorkers,
		qps,
		etaStr,
		available,
		processed,
		total,
	)

	// 清除旧内容并输出新状态
	r.clearLine()
	fmt.Print(status)
	r.lastLineLen = len(status)
}

// clearLine 清除当前行
func (r *StatusRenderer) clearLine() {
	if r.lastLineLen > 0 {
		fmt.Printf("\r%s\r", strings.Repeat(" ", r.lastLineLen))
	}
}

// formatDuration 格式化时间显示
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "--:--"
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
