package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type ProxyPool struct {
	proxies []string
	index   atomic.Int64
}

func loadProxies(path string) *ProxyPool {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		logf("Warning: could not open proxy file %s: %v", path, err)
		return nil
	}
	defer f.Close()

	var proxies []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxies = append(proxies, line)
	}

	if len(proxies) == 0 {
		return nil
	}

	logf("Loaded %d proxies from %s", len(proxies), path)
	return &ProxyPool{proxies: proxies}
}

func (p *ProxyPool) next() string {
	if p == nil || len(p.proxies) == 0 {
		return ""
	}
	i := p.index.Add(1) - 1
	return p.proxies[i%int64(len(p.proxies))]
}

func (p *ProxyPool) random() string {
	if p == nil || len(p.proxies) == 0 {
		return ""
	}
	return p.proxies[rand.Intn(len(p.proxies))]
}

func buildMonitorClient(pool *ProxyPool) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 0,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if pool != nil {
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			proxy := pool.random()
			if proxy == "" {
				return nil, nil
			}
			if !strings.Contains(proxy, "://") {
				proxy = "http://" + proxy
			}
			return url.Parse(proxy)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// monitorForSlots polls /4/find until slots appear, then returns them.
// Uses proxy rotation to avoid rate limits on any single IP.
func monitorForSlots(client *http.Client, venueID int, date, targetTime string, partySize int, interval int) []Slot {
	findURL := buildFindURL(venueID, date, partySize)

	logf("Monitor mode: polling every %ds for venue %d on %s", interval, venueID, date)

	backoff := time.Duration(interval) * time.Second
	maxBackoff := 5 * time.Minute

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", findURL, nil)
		setResyHeaders(req)

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			logf("Monitor: request error: %v — backing off", err)
			backoff = min(backoff*2, maxBackoff)
			time.Sleep(backoff + safeJitter(backoff))
			continue
		}

		body, _ := readBody(resp)
		cancel()

		switch resp.StatusCode {
		case 200:
			slots := parseSlots(body, targetTime)
			if len(slots) > 0 {
				logf("Monitor: found %d matching slots!", len(slots))
				return slots
			}
			logf("Monitor: 200 OK but no matching slots — retrying in %s", backoff)

		case 429, 403:
			backoff = min(backoff*2, maxBackoff)
			logf("Monitor: rate limited (HTTP %d) — backing off to %s", resp.StatusCode, backoff)

		case 401:
			fatal("Monitor: auth token expired (HTTP 401). Re-run with fresh credentials.")

		default:
			logf("Monitor: unexpected HTTP %d — retrying", resp.StatusCode)
		}

		time.Sleep(backoff + safeJitter(backoff))

		// Reset backoff on successful request (no rate limit)
		if resp.StatusCode == 200 {
			backoff = time.Duration(interval) * time.Second
		}
	}
}

// baseHeaders is pre-built once at startup — avoids 9-11 Header.Set() calls per request.
// Cloned via .Clone() in the hot path (~200ns vs ~1µs for Set() x 11).
var baseHeaders http.Header

func initBaseHeaders() {
	baseHeaders = http.Header{
		"Authorization":   {resyAPIKey},
		"User-Agent":      {userAgent},
		"Accept":          {"application/json, text/plain, */*"},
		"Accept-Encoding": {"gzip, deflate, br"},
		"Origin":          {"https://resy.com"},
		"Referer":         {"https://resy.com/"},
		"X-Origin":        {"https://resy.com"},
		"Cache-Control":   {"no-cache"},
		"Dnt":             {"1"},
	}
	if authHeader != "" {
		baseHeaders["X-Resy-Auth-Token"] = []string{authHeader}
		baseHeaders["X-Resy-Universal-Auth"] = []string{authHeader}
	}
}

func setResyHeaders(req *http.Request) {
	if baseHeaders != nil {
		req.Header = baseHeaders.Clone()
	} else {
		// Fallback for calls before initBaseHeaders (warmup, etc.)
		req.Header.Set("Authorization", resyAPIKey)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		req.Header.Set("Origin", "https://resy.com")
		req.Header.Set("Referer", "https://resy.com/")
		req.Header.Set("X-Origin", "https://resy.com")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("DNT", "1")
		if authHeader != "" {
			req.Header.Set("X-Resy-Auth-Token", authHeader)
			req.Header.Set("X-Resy-Universal-Auth", authHeader)
		}
	}
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return readCompressedBody(resp)
}

// safeJitter returns a random ±10% jitter of the given duration.
// Guards against rand.Intn(0) panic when duration is very small.
func safeJitter(d time.Duration) time.Duration {
	r := int(d / 10)
	if r <= 0 {
		return 0
	}
	j := time.Duration(rand.Intn(r))
	if rand.Intn(2) == 0 {
		j = -j
	}
	return j
}

// warmConnections primes the TCP+TLS connection to api.resy.com.
// With HTTP/2, all requests multiplex over ONE connection, so we only
// need 1 warmup request. Uses /3/geoip (lightest endpoint, ~50 bytes)
// instead of /4/find (87KB) to avoid wasting rate limit budget.
// Then fires one real POST /4/find to prime that specific HTTP/2 stream.
func warmConnections(client *http.Client, n int, findURL string, findBody []byte) {
	logf("Warming connection...")
	start := time.Now()

	// Step 1: lightest endpoint to establish TCP+TLS+H2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.resy.com/3/geoip", nil)
	setResyHeaders(req)
	resp, err := client.Do(req)
	if err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	cancel()

	// Step 2: one real POST /4/find to prime the exact endpoint path
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	req2, _ := http.NewRequestWithContext(ctx2, "POST", "https://api.resy.com/4/find", bytes.NewReader(findBody))
	setResyHeaders(req2)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err == nil {
		io.ReadAll(resp2.Body)
		resp2.Body.Close()
	}
	cancel2()

	logf("Connection warmed (%v)", time.Since(start).Round(time.Millisecond))
}

// pinnedDialer creates a dialer that resolves DNS once and reuses the IP.
func pinnedDialer(pinnedIP string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if pinnedIP != "" {
			_, port, _ := net.SplitHostPort(addr)
			addr = net.JoinHostPort(pinnedIP, port)
		}
		return dialer.DialContext(ctx, network, addr)
	}
}
