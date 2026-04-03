package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"math/rand"
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

		case 401, 419:
			fatal("Monitor: auth token rejected (HTTP %d). Re-run with fresh credentials.", resp.StatusCode)

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
	// Chrome Client Hint headers — Imperva checks these for consistency
	// with the TLS fingerprint. Missing them when JA3 says Chrome = detection.
	hints := chromeClientHintHeaders()

	baseHeaders = http.Header{
		"Authorization":      {resyAPIKey},
		"User-Agent":         {userAgent},
		"Accept":             {"application/json, text/plain, */*"},
		"Accept-Encoding":    {"gzip, deflate, br, zstd"},
		"Accept-Language":    {"en,en-US;q=0.9"},
		"Origin":             {"https://widgets.resy.com"},
		"Referer":            {"https://widgets.resy.com/"},
		"X-Origin":           {"https://widgets.resy.com"},
		"Cache-Control":      {"no-cache"},
		"Dnt":                {"1"},
		"Priority":           {"u=1, i"},
		"Sec-CH-UA":          {hints["Sec-CH-UA"]},
		"Sec-CH-UA-Mobile":   {hints["Sec-CH-UA-Mobile"]},
		"Sec-CH-UA-Platform": {hints["Sec-CH-UA-Platform"]},
		"Sec-Fetch-Dest":     {"empty"},
		"Sec-Fetch-Mode":     {"cors"},
		"Sec-Fetch-Site":     {"same-site"},
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
		req.Header.Set("Origin", "https://widgets.resy.com")
		req.Header.Set("Referer", "https://widgets.resy.com/")
		req.Header.Set("X-Origin", "https://widgets.resy.com")
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

// warmConnections primes the connection to api.resy.com and collects
// Imperva session cookies (visid_incap, nlbi, incap_ses).
//
// Step 1: GET /3/geoip — lightest endpoint, establishes TLS + collects cookies
// Step 2: POST /4/find — primes the exact endpoint path
//
// The cookies are stored in the http.Client's cookie jar and sent back
// on all subsequent requests. This gives Imperva an established session
// baseline before the critical monitoring window begins.
// No reese84/utmvc cookies are served on API endpoints (confirmed by testing).
func warmConnections(client *http.Client, n int, findURL string, findBody []byte) {
	logf("Warming connection + collecting Imperva session cookies...")
	start := time.Now()

	// Step 1: GET /3/geoip — collect Imperva cookies (visid_incap, nlbi, incap_ses)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.resy.com/3/geoip", nil)
	setResyHeaders(req)
	resp, err := client.Do(req)
	if err == nil {
		// Log Imperva cookies from Set-Cookie headers
		var cookies []string
		// Try canonical key first, then lowercase (fhttp may use either)
		for _, key := range []string{"Set-Cookie", "set-cookie"} {
			for _, sc := range resp.Header.Values(key) {
				if i := strings.IndexByte(sc, '='); i > 0 {
					cookies = append(cookies, sc[:i])
				}
			}
		}
		if len(cookies) > 0 {
			logf("Imperva cookies: %v", cookies)
		} else {
			logf("Warmup geoip: HTTP %d (no Set-Cookie headers found, %d header keys total)", resp.StatusCode, len(resp.Header))
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		logf("Warmup geoip: %v", err)
	}
	cancel()

	// Step 2: GET /2/user — prime the authenticated session (Chrome does this before booking).
	// This tells the server "this user is active" and may establish payment bypass state.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	req2, _ := http.NewRequestWithContext(ctx2, "GET", "https://api.resy.com/2/user", nil)
	setResyHeaders(req2)
	resp2, err := client.Do(req2)
	if err == nil {
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		logf("Warmup /2/user: HTTP %d", resp2.StatusCode)
	} else {
		logf("Warmup /2/user: %v", err)
	}
	cancel2()

	// Step 3: POST /4/find — prime the exact endpoint path
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
	req3, _ := http.NewRequestWithContext(ctx3, "POST", "https://api.resy.com/4/find", bytes.NewReader(findBody))
	setResyHeaders(req3)
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := client.Do(req3)
	if err == nil {
		io.Copy(io.Discard, resp3.Body)
		resp3.Body.Close()
	} else {
		logf("Warmup find: %v", err)
	}
	cancel3()

	logf("Connection warmed (%v)", time.Since(start).Round(time.Millisecond))
}

