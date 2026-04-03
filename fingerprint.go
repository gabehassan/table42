package main

// Chrome browser fingerprint impersonation for bypassing Imperva WAF.
//
// Uses bogdanfinn/tls-client to produce Chrome-identical fingerprints across
// ALL detection vectors: TLS (JA3/JA4), HTTP/2 SETTINGS frame, pseudo-header
// ordering, connection flow control, and header ordering.
//
// Architecture: each proxy gets its own dedicated client (buildStickyClient).
// No shared SetProxy calls, no mutex, no race condition. At startup,
// benchmarkProxies tests all proxies and assigns roles by speed.

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// chromeTransport adapts bogdanfinn/tls-client as a standard http.RoundTripper.
// Each instance is pinned to a single proxy (or direct). No proxy rotation.
type chromeTransport struct {
	client tls_client.HttpClient
}

func (t *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fReq, err := t.buildFReq(req)
	if err != nil {
		return nil, err
	}

	fResp, err := t.client.Do(fReq)
	if err != nil {
		return nil, err
	}

	return t.convertResp(req, fResp), nil
}

func (t *chromeTransport) buildFReq(req *http.Request) (*fhttp.Request, error) {
	fReq, err := fhttp.NewRequest(req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}
	fReq = fReq.WithContext(req.Context())

	for key, vals := range req.Header {
		for _, v := range vals {
			fReq.Header.Add(key, v)
		}
	}
	fReq.ContentLength = req.ContentLength

	// Chrome header order — matches real Chrome HAR capture
	fReq.Header[fhttp.HeaderOrderKey] = []string{
		"accept",
		"accept-encoding",
		"accept-language",
		"authorization",
		"cache-control",
		"content-length",
		"content-type",
		"dnt",
		"origin",
		"priority",
		"referer",
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"sec-fetch-dest",
		"sec-fetch-mode",
		"sec-fetch-site",
		"user-agent",
		"x-origin",
		"x-resy-auth-token",
		"x-resy-universal-auth",
	}

	// Chrome pseudo-header order
	fReq.Header[fhttp.PHeaderOrderKey] = []string{
		":method",
		":authority",
		":scheme",
		":path",
	}

	return fReq, nil
}

func (t *chromeTransport) convertResp(req *http.Request, fResp *fhttp.Response) *http.Response {
	return &http.Response{
		Status:        fResp.Status,
		StatusCode:    fResp.StatusCode,
		Proto:         fResp.Proto,
		ProtoMajor:    fResp.ProtoMajor,
		ProtoMinor:    fResp.ProtoMinor,
		Header:        http.Header(fResp.Header),
		Body:          fResp.Body,
		ContentLength: fResp.ContentLength,
		Uncompressed:  fResp.Uncompressed,
		Request:       req, // Required for http.Client cookie jar to store cookies
	}
}

// ───────────────────────────── Proxy benchmarking ─────────────────────────────

// ProxyBenchResult holds the benchmark result for a single proxy.
type ProxyBenchResult struct {
	ProxyURL string
	ColdRTT  time.Duration
	WarmRTT  time.Duration
	OK       bool
}

// benchmarkProxies tests all proxies in parallel (cold + warm) and returns
// results sorted by warm RTT, with outliers removed.
func benchmarkProxies(proxies []string) []ProxyBenchResult {
	if len(proxies) == 0 {
		return nil
	}

	logf("Benchmarking %d proxies against api.resy.com...", len(proxies))
	results := make([]ProxyBenchResult, len(proxies))
	var wg sync.WaitGroup

	for i, proxy := range proxies {
		wg.Add(1)
		go func(idx int, proxyURL string) {
			defer wg.Done()
			results[idx] = benchmarkOneProxy(proxyURL)
		}(i, proxy)
	}
	wg.Wait()

	// Collect passing results
	var passing []ProxyBenchResult
	for _, r := range results {
		if r.OK {
			passing = append(passing, r)
		} else {
			logf("  FAIL: %s", truncateProxy(r.ProxyURL))
		}
	}

	if len(passing) == 0 {
		logf("WARNING: All proxies failed benchmark!")
		return nil
	}

	// Sort by warm RTT
	sort.Slice(passing, func(i, j int) bool {
		return passing[i].WarmRTT < passing[j].WarmRTT
	})

	// Calculate median warm RTT
	median := passing[len(passing)/2].WarmRTT

	// Remove outliers: >2x median or >800ms absolute
	var good []ProxyBenchResult
	for _, r := range passing {
		if r.WarmRTT > 800*time.Millisecond || r.WarmRTT > 2*median {
			logf("  OUTLIER: %s warm=%v (median=%v)", truncateProxy(r.ProxyURL), r.WarmRTT.Round(time.Millisecond), median.Round(time.Millisecond))
		} else {
			good = append(good, r)
			logf("  OK: %s cold=%v warm=%v", truncateProxy(r.ProxyURL), r.ColdRTT.Round(time.Millisecond), r.WarmRTT.Round(time.Millisecond))
		}
	}

	logf("Proxy benchmark: %d/%d passed, %d usable (median warm=%v)", len(passing), len(proxies), len(good), median.Round(time.Millisecond))
	return good
}

// benchmarkOneProxy tests a single proxy with a cold and warm request.
func benchmarkOneProxy(proxyURL string) ProxyBenchResult {
	result := ProxyBenchResult{ProxyURL: proxyURL}

	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithTimeoutSeconds(15),
		tls_client.WithProxyUrl(proxyURL),
		tls_client.WithInsecureSkipVerify(),
	}
	tc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return result
	}

	// Cold request
	t0 := time.Now()
	req, _ := fhttp.NewRequest("GET", "https://api.resy.com/3/geoip", nil)
	req.Header.Set("Authorization", resyAPIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := tc.Do(req)
	if err != nil {
		return result
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return result
	}
	result.ColdRTT = time.Since(t0)

	// Warm request (connection reuse)
	t1 := time.Now()
	req2, _ := fhttp.NewRequest("GET", "https://api.resy.com/3/geoip", nil)
	req2.Header.Set("Authorization", resyAPIKey)
	req2.Header.Set("Accept", "application/json")
	resp2, err := tc.Do(req2)
	if err != nil {
		return result
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return result
	}
	result.WarmRTT = time.Since(t1)
	result.OK = true
	return result
}

// truncateProxy returns a short display string for a proxy URL (hides password).
func truncateProxy(proxyURL string) string {
	// Show host:port only
	if at := strings.LastIndex(proxyURL, "@"); at >= 0 {
		return proxyURL[at+1:]
	}
	return proxyURL
}

// ───────────────────────────── Client builders ─────────────────────────────

// buildStickyClient creates a Chrome-fingerprinted http.Client pinned to one proxy.
// Each client has its own TLS session, cookie jar, and Imperva session.
func buildStickyClient(proxyURL string) *http.Client {
	options := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithTimeoutSeconds(10),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithInsecureSkipVerify(),
	}
	if proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(proxyURL))
	}

	tlsClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		logf("Warning: tls-client init failed for %s: %v", truncateProxy(proxyURL), err)
		return buildFallbackClient()
	}

	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: &chromeTransport{client: tlsClient},
		Jar:       jar,
		Timeout:   0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func buildFallbackClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ───────────────────────────── Proxy loading ─────────────────────────────

// loadProxyList loads proxies from a file (one URL per line).
func loadProxyList(path string) []string {
	if path == "" {
		for _, p := range []string{"proxylist.txt", os.Getenv("RESY_PROXY_FILE")} {
			if p != "" {
				if _, err := os.Stat(p); err == nil {
					path = p
					break
				}
			}
		}
	}
	if path == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
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
		if !strings.Contains(line, "://") {
			parts := strings.SplitN(line, ":", 4)
			if len(parts) == 4 {
				line = fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
			} else if len(parts) == 2 {
				line = "http://" + line
			}
		}
		proxies = append(proxies, line)
	}
	return proxies
}

// ───────────────────────────── Client Hint headers ─────────────────────────────

func chromeClientHintHeaders() map[string]string {
	ver := "146"
	if i := strings.Index(userAgent, "Chrome/"); i >= 0 {
		s := userAgent[i+7:]
		if j := strings.IndexByte(s, '.'); j > 0 {
			ver = s[:j]
		}
	}
	return map[string]string{
		"Sec-CH-UA":          `"Chromium";v="` + ver + `", "Not-A.Brand";v="24", "Google Chrome";v="` + ver + `"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"macOS"`,
	}
}
