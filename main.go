package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"domain_scanner/internal/generator"
	"domain_scanner/internal/stats"
	"domain_scanner/internal/types"
	"domain_scanner/internal/worker"
)

func printHelp() {
	fmt.Println("Domain Scanner - A tool to check domain availability")
	fmt.Println("\nUsage:")
	fmt.Println("  go run main.go [options]")
	fmt.Println("\nOptions:")
	fmt.Println("  -l int      Domain length (default: 3)")
	fmt.Println("  -s string   Domain suffix (default: .li)")
	fmt.Println("  -p string   Domain pattern:")
	fmt.Println("              d: Pure numbers (e.g., 123.li)")
	fmt.Println("              D: Pure letters (e.g., abc.li)")
	fmt.Println("              a: Alphanumeric (e.g., a1b.li)")
	fmt.Println("  -r string   Regex filter for domain name prefix")
	fmt.Println("  -dict string Dictionary file path (one word per line)")
	fmt.Println("  -delay int  Delay between queries in milliseconds (default: 1000)")
	fmt.Println("  -workers int Number of concurrent workers (default: 10)")
	fmt.Println("  -show-registered Show registered domains in output (default: false)")
	fmt.Println("  -force      Skip performance warnings for large domain sets (default: false)")
	fmt.Println("  -h          Show help information")
	fmt.Println("\nExamples:")
	fmt.Println("  1. Check 3-letter .li domains with 20 workers:")
	fmt.Println("     go run main.go -l 3 -s .li -p D -workers 20")
	fmt.Println("\n  2. Check domains with custom delay and workers:")
	fmt.Println("     go run main.go -l 3 -s .li -p D -delay 500 -workers 15")
	fmt.Println("\n  3. Show both available and registered domains:")
	fmt.Println("     go run main.go -l 3 -s .li -p D -show-registered")
	fmt.Println("\n  4. Use regex filter to match domain prefix:")
	fmt.Println("     go run main.go -l 3 -s .li -p D -r \"^[a-z]{2}[0-9]$\"")
	fmt.Println("\n  5. Find domains starting with specific letters:")
	fmt.Println("     go run main.go -l 5 -s .li -p D -r \"^abc\"")
	fmt.Println("\n  6. Use dictionary file to check word-based domains:")
	fmt.Println("     go run main.go -dict words.txt -s .com")
	fmt.Println("\n  7. Use dictionary with regex filter:")
	fmt.Println("     go run main.go -dict words.txt -s .com -r \"^[a-z]{4,8}$\"")
	fmt.Println("\n  8. Skip performance warning for large domain sets:")
	fmt.Println("     go run main.go -l 7 -s .li -p D -force")
}

func showPerformanceWarning(length int, pattern string, delay int, workers int) {
	var charsetSize int
	switch pattern {
	case "d":
		charsetSize = 10 // 0-9
	case "D":
		charsetSize = 26 // a-z
	case "a":
		charsetSize = 36 // a-z + 0-9
	default:
		charsetSize = 26
	}

	totalDomains := 1
	for i := 0; i < length; i++ {
		totalDomains *= charsetSize
	}

	// 估算时间（基于延迟和worker数）
	estimatedSeconds := (totalDomains * delay) / (workers * 1000)
	estimatedHours := estimatedSeconds / 3600
	estimatedDays := estimatedHours / 24

	fmt.Println("\n\033[1;33m⚠️  PERFORMANCE WARNING ⚠️\033[0m")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("You are about to scan \033[1;31m%d domains\033[0m with the following settings:\n", totalDomains)
	fmt.Printf("• Pattern: %s (charset size: %d)\n", pattern, charsetSize)
	fmt.Printf("• Length: %d characters\n", length)
	fmt.Printf("• Workers: %d\n", workers)
	fmt.Printf("• Delay: %d ms between queries\n", delay)
	fmt.Println()

	fmt.Println("📊 \033[1;36mEstimated Impact:\033[0m")
	if estimatedDays >= 1 {
		fmt.Printf("• Scan time: ~%.1f days (%.1f hours)\n", float64(estimatedDays), float64(estimatedHours))
	} else if estimatedHours >= 1 {
		fmt.Printf("• Scan time: ~%.1f hours (%.0f minutes)\n", float64(estimatedHours), float64(estimatedHours)*60)
	} else {
		fmt.Printf("• Scan time: ~%.0f minutes\n", float64(estimatedSeconds)/60)
	}
	fmt.Printf("• Network requests: %d total\n", totalDomains)
	fmt.Printf("• Memory usage: High (processing %d domains)\n", totalDomains)
	fmt.Println()

	fmt.Println("💡 \033[1;32mRecommendations:\033[0m")
	fmt.Println("• Use regex filter (-r) to narrow down the search")
	fmt.Println("• Consider shorter domain length (-l)")
	fmt.Println("• Increase workers (-workers) for faster processing")
	fmt.Println("• Decrease delay (-delay) if your network can handle it")
	fmt.Println("• Use -force flag to skip this warning next time")
	fmt.Println("═══════════════════════════════════════════════════════")
}

func confirmContinue() bool {
	fmt.Print("\nDo you want to continue? (y/N): ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

func showMOTD() {
	fmt.Println("\033[1;36m") // Cyan color
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    Domain Scanner v1.3.4                   ║")
	fmt.Println("║                                                            ║")
	fmt.Println("║  A powerful tool for checking domain name availability     ║")
	fmt.Println("║                                                            ║")
	fmt.Println("║  Developer: www.ict.run                                    ║")
	fmt.Println("║  GitHub:    https://github.com/xuemian168/domain-scanner   ║")
	fmt.Println("║                                                            ║")
	fmt.Println("║  License:   AGPL-3.0                                       ║")
	fmt.Println("║  Copyright © 2025                                          ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("\033[0m") // Reset color
	fmt.Println()
}

func main() {
	// Show MOTD
	showMOTD()

	// Define command line flags
	length := flag.Int("l", 3, "Domain length")
	suffix := flag.String("s", ".li", "Domain suffix")
	pattern := flag.String("p", "D", "Domain pattern (d: numbers, D: letters, a: alphanumeric)")
	regexFilter := flag.String("r", "", "Regex filter for domain names")
	dictFile := flag.String("dict", "", "Dictionary file path (one word per line)")
	delay := flag.Int("delay", 1000, "Delay between queries in milliseconds")
	workers := flag.Int("workers", 10, "Number of concurrent workers")
	showRegistered := flag.Bool("show-registered", false, "Show registered domains in output")
	force := flag.Bool("force", false, "Skip performance warnings for large domain sets")
	help := flag.Bool("h", false, "Show help information")
	flag.Parse()

	if *help {
		printHelp()
		os.Exit(0)
	}

	// Ensure suffix starts with a dot
	if !strings.HasPrefix(*suffix, ".") {
		*suffix = "." + *suffix
	}

	// Validate input modes
	if *dictFile != "" && (*length != 3 || *pattern != "D") {
		// Dictionary mode: length and pattern are ignored, but inform user
		if *length != 3 || *pattern != "D" {
			fmt.Printf("Note: When using dictionary mode, -l and -p parameters are ignored\n")
		}
	} else if *dictFile != "" {
		// Pure dictionary mode
	} else {
		// Traditional pattern mode - apply performance warning
		if *length > 5 && !*force {
			showPerformanceWarning(*length, *pattern, *delay, *workers)
			if !confirmContinue() {
				fmt.Println("Scan cancelled by user.")
				os.Exit(0)
			}
			fmt.Println()
		}
	}

	domainGen := generator.GenerateDomains(*length, *suffix, *pattern, *regexFilter, *dictFile)
	domainChan := domainGen.Domains
	availableDomains := []string{}
	registeredDomains := []string{}

	// 获取预估域名数量
	estimatedDomains := domainGen.TotalCount
	fmt.Printf("Checking estimated %d domains with pattern %s and length %d using %d workers...\n",
		estimatedDomains, *pattern, *length, *workers)
	if *regexFilter != "" {
		fmt.Printf("Using regex filter: %s\n", *regexFilter)
	}

	// 创建统计收集器和状态渲染器
	collector := stats.NewCollector(int64(estimatedDomains), *workers)
	renderer := stats.NewStatusRenderer(collector)
	renderer.Start()

	// Create channels for jobs and results
	jobs := make(chan string, 1000)
	results := make(chan types.DomainResult, 1000)

	// Start workers with WaitGroup
	var workerWg sync.WaitGroup
	for w := 1; w <= *workers; w++ {
		workerWg.Add(1)
		go func(id int) {
			defer workerWg.Done()
			worker.Worker(id, jobs, results, time.Duration(*delay)*time.Millisecond, collector)
		}(w)
	}

	// Send jobs from domain generator
	go func() {
		defer close(jobs)
		for domain := range domainChan {
			jobs <- domain
		}
	}()

	// Collect results
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		isTTY := renderer.IsTTY()
		for result := range results {
			collector.IncrementProcessed()

			if result.Error != nil {
				// 错误情况在终端模式下不显示，避免干扰状态栏
				if !isTTY {
					fmt.Printf("[ERROR] %s: %v\n", result.Domain, result.Error)
				}
				continue
			}

			if result.Available {
				collector.IncrementAvailable()
				availableDomains = append(availableDomains, result.Domain)
				// 打印可用域名
				if isTTY {
					fmt.Printf("\r\033[K[FOUND] %s is AVAILABLE!\n", result.Domain)
				} else {
					fmt.Printf("[FOUND] %s is AVAILABLE!\n", result.Domain)
				}
			} else if *showRegistered {
				sigStr := strings.Join(result.Signatures, ", ")
				registeredDomains = append(registeredDomains, result.Domain)
				if !isTTY {
					fmt.Printf("[REG] %s [%s]\n", result.Domain, sigStr)
				}
			}
		}
	}()

	// 监控任务完成 - 等待所有worker完成后关闭results
	go func() {
		// 等待所有worker完成
		workerWg.Wait()

		// 关闭results channel，结束结果收集
		close(results)
	}()

	wg.Wait()

	// 停止状态渲染器
	renderer.Stop()

	// Save available domains to file
	availableFile := fmt.Sprintf("available_domains_%s_%d_%s.txt", *pattern, *length, strings.TrimPrefix(*suffix, "."))
	file, err := os.Create(availableFile)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	for _, domain := range availableDomains {
		_, err := file.WriteString(domain + "\n")
		if err != nil {
			fmt.Printf("Error writing to file: %v\n", err)
			os.Exit(1)
		}
	}

	// Save registered domains to file only if show-registered is true
	registeredFile := fmt.Sprintf("registered_domains_%s_%d_%s.txt", *pattern, *length, strings.TrimPrefix(*suffix, "."))
	if *showRegistered {
		regFile, err := os.Create(registeredFile)
		if err != nil {
			fmt.Printf("Error creating registered domains file: %v\n", err)
			os.Exit(1)
		}
		defer regFile.Close()

		for _, domain := range registeredDomains {
			_, err := regFile.WriteString(domain + "\n")
			if err != nil {
				fmt.Printf("Error writing to registered domains file: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// 获取实际生成的域名数量
	actualDomainsGenerated := atomic.LoadInt64(domainGen.Generated)
	actualDomainsChecked := int(actualDomainsGenerated)

	fmt.Printf("\n\nResults saved to:\n")
	fmt.Printf("- Available domains: %s\n", availableFile)
	if *showRegistered {
		fmt.Printf("- Registered domains: %s\n", registeredFile)
	}
	fmt.Printf("\nSummary:\n")
	fmt.Printf("- Total domains checked: %d\n", actualDomainsChecked)
	fmt.Printf("- Available domains: %d\n", len(availableDomains))
	if *showRegistered {
		fmt.Printf("- Registered domains: %d\n", len(registeredDomains))
	}
}
