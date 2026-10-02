package worker

import (
	"time"

	"domain_scanner/internal/domain"
	"domain_scanner/internal/stats"
	"domain_scanner/internal/types"
)

// Worker 域名检查工作协程
func Worker(id int, jobs <-chan string, results chan<- types.DomainResult, delay time.Duration, collector *stats.Collector) {
	for domainName := range jobs {
		// 标记 Worker 开始工作
		if collector != nil {
			collector.IncrementActiveWorkers()
		}

		available, err := domain.CheckDomainAvailability(domainName)
		signatures, _ := domain.CheckDomainSignatures(domainName)

		results <- types.DomainResult{
			Domain:     domainName,
			Available:  available,
			Error:      err,
			Signatures: signatures,
		}

		// 标记 Worker 完成当前任务
		if collector != nil {
			collector.DecrementActiveWorkers()
		}

		time.Sleep(delay)
	}
}
