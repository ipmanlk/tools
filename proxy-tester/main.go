package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// fetchProxies downloads proxies from the provided URL.
func fetchProxies(rawURL string) ([]string, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from URL %s: %v", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("non-OK HTTP status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %v", err)
	}

	lines := strings.Split(string(body), "\n")
	var proxies []string
	for _, line := range lines {
		proxyStr := strings.TrimSpace(line)
		if proxyStr != "" {
			proxies = append(proxies, proxyStr)
		}
	}
	return proxies, nil
}

// detectDefaultScheme returns the proxy scheme based on the file name of the input URL.
// It uses the portion of the file name before the extension.
func detectDefaultScheme(inputURL string) (string, error) {
	u, err := url.Parse(inputURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %v", err)
	}

	base := strings.ToLower(path.Base(u.Path))
	ext := path.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}

	// Return based on the file name.
	switch base {
	case "https":
		return "https", nil
	case "http":
		return "http", nil
	case "socks4":
		return "socks4", nil
	case "socks5":
		return "socks5", nil
	default:
		return "http", nil
	}
}

// testProxy tests the provided proxy URL by performing an HTTP GET request to a test endpoint.
func testProxy(proxyStr string) bool {
	u, err := url.Parse(proxyStr)
	if err != nil {
		fmt.Printf("Failed to parse proxy '%s': %v\n", proxyStr, err)
		return false
	}

	// Set up a client with a timeout.
	client := &http.Client{
		Timeout: 12 * time.Second,
	}

	// Configure transport based on the proxy scheme.
	switch u.Scheme {
	case "socks5", "socks4":
		// For SOCKS proxies, use golang.org/x/net/proxy.
		dialer, err := proxy.SOCKS5("tcp", u.Host, nil, proxy.Direct)
		if err != nil {
			fmt.Printf("Error creating %s dialer for '%s': %v\n", u.Scheme, proxyStr, err)
			return false
		}
		transport := &http.Transport{
			Dial: dialer.Dial,
		}
		client.Transport = transport

	case "http", "https":
		transport := &http.Transport{
			Proxy: http.ProxyURL(u),
		}
		client.Transport = transport

	default:
		fmt.Printf("Unsupported proxy scheme '%s' in '%s'\n", u.Scheme, proxyStr)
		return false
	}

	// Test URL that returns our IP address.
	resp, err := client.Get("https://ifconfig.me/ip")
	if err != nil {
		fmt.Printf("Request failed for proxy '%s': %v\n", proxyStr, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Non-OK status for proxy '%s': %d\n", proxyStr, resp.StatusCode)
		return false
	}

	return true
}

func main() {
	// Define a flag for concurrency (cost).
	cost := flag.Int("cost", 10, "Number of concurrent proxy tests")
	flag.Parse()

	// The remaining arguments are URLs to proxy lists.
	urls := flag.Args()
	if len(urls) < 1 {
		fmt.Println("Usage: go run main.go -cost=<number> <proxy_list_url1> [<proxy_list_url2> ...]")
		return
	}

	// Collect proxies from all provided URLs.
	var allProxies []string
	for _, arg := range urls {
		defaultScheme, err := detectDefaultScheme(arg)
		if err != nil {
			fmt.Printf("Error detecting default scheme: %v\n", err)
			continue
		}
		fmt.Printf("Fetching proxies from: %s (default scheme: %s)\n", arg, defaultScheme)
		proxies, err := fetchProxies(arg)
		if err != nil {
			fmt.Printf("Error fetching proxies: %v\n", err)
			continue
		}
		// Prepend the default scheme if missing.
		for _, p := range proxies {
			if !strings.Contains(p, "://") {
				p = defaultScheme + "://" + p
			}
			allProxies = append(allProxies, p)
		}
	}

	if len(allProxies) == 0 {
		fmt.Println("No proxies found from the provided URLs.")
		return
	}

	fmt.Printf("Total proxies fetched: %d\n", len(allProxies))

	// Channel to feed proxies to workers.
	proxyCh := make(chan string)
	// Channel to collect working proxies.
	workingCh := make(chan string)

	var wg sync.WaitGroup

	// Start worker goroutines.
	for i := 0; i < *cost; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxyStr := range proxyCh {
				fmt.Printf("Testing proxy: %s\n", proxyStr)
				if testProxy(proxyStr) {
					fmt.Printf("Proxy works: %s\n", proxyStr)
					workingCh <- proxyStr
				} else {
					fmt.Printf("Proxy failed: %s\n", proxyStr)
				}
			}
		}()
	}

	// Feed proxies to the channel.
	go func() {
		for _, p := range allProxies {
			proxyCh <- p
		}
		close(proxyCh)
	}()

	// Wait for all workers to finish then close workingCh.
	go func() {
		wg.Wait()
		close(workingCh)
	}()

	// Collect working proxies.
	var workingProxies []string
	for wp := range workingCh {
		workingProxies = append(workingProxies, wp)
	}

	// Write the working proxies to a file named "working_proxies".
	outFileName := "working_proxies"
	outFile, err := os.Create(outFileName)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		return
	}
	defer outFile.Close()

	for _, wp := range workingProxies {
		_, err := outFile.WriteString(wp + "\n")
		if err != nil {
			fmt.Printf("Error writing to output file: %v\n", err)
			return
		}
	}

	fmt.Printf("Working proxies have been written to %s\n", outFileName)
}
