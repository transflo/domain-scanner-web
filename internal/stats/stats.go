package stats

import (
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// Collector 实时统计收集器
type Collector struct {
	totalDomains   int64
	processedCount int64
	availableCount int64
	activeWorkers  int64
	totalWorkers   int
	startTime      time.Time
}

// NewCollector 创建新的统计收集器
func NewCollector(totalDomains int64, totalWorkers int) *Collector {
	return &Collector{
		totalDomains: totalDomains,
		totalWorkers: totalWorkers,
		startTime:    time.Now(),
	}
}

// IncrementProcessed 增加已处理计数
func (c *Collector) IncrementProcessed() {
	atomic.AddInt64(&c.processedCount, 1)
}

// IncrementAvailable 增加可用域名计数
func (c *Collector) IncrementAvailable() {
	atomic.AddInt64(&c.availableCount, 1)
}

// IncrementActiveWorkers 增加活跃 Worker 计数
func (c *Collector) IncrementActiveWorkers() {
	atomic.AddInt64(&c.activeWorkers, 1)
}

// DecrementActiveWorkers 减少活跃 Worker 计数
func (c *Collector) DecrementActiveWorkers() {
	atomic.AddInt64(&c.activeWorkers, -1)
}

// GetProcessedCount 获取已处理数量
func (c *Collector) GetProcessedCount() int64 {
	return atomic.LoadInt64(&c.processedCount)
}

// GetAvailableCount 获取可用域名数量
func (c *Collector) GetAvailableCount() int64 {
	return atomic.LoadInt64(&c.availableCount)
}

// GetActiveWorkers 获取活跃 Worker 数量
func (c *Collector) GetActiveWorkers() int64 {
	return atomic.LoadInt64(&c.activeWorkers)
}

// GetTotalWorkers 获取总 Worker 数量
func (c *Collector) GetTotalWorkers() int {
	return c.totalWorkers
}

// GetTotalDomains 获取总域名数量
func (c *Collector) GetTotalDomains() int64 {
	return c.totalDomains
}

// CalculateQPS 计算每秒处理速率
func (c *Collector) CalculateQPS() float64 {
	elapsed := time.Since(c.startTime).Seconds()
	if elapsed < 1 {
		return 0
	}
	processed := atomic.LoadInt64(&c.processedCount)
	return float64(processed) / elapsed
}

// CalculateETA 计算预估剩余时间
func (c *Collector) CalculateETA() time.Duration {
	qps := c.CalculateQPS()
	if qps < 0.001 {
		return 0
	}
	processed := atomic.LoadInt64(&c.processedCount)
	remaining := c.totalDomains - processed
	if remaining <= 0 {
		return 0
	}
	return time.Duration(float64(remaining)/qps) * time.Second
}

// GetProgress 获取进度百分比
func (c *Collector) GetProgress() float64 {
	if c.totalDomains == 0 {
		return 0
	}
	processed := atomic.LoadInt64(&c.processedCount)
	return float64(processed) / float64(c.totalDomains) * 100
}

// IsTerminal 检测是否为终端环境
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
