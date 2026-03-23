//go:build ignore

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Duplicated constants (build tag: ignore, separate compilation)
const benchResyAPIKey = `ResyAPI api_key="VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"`
const benchUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

type traceResult struct {
	DNS     time.Duration
	TCP     time.Duration
	TLS     time.Duration
	TTFB    time.Duration
	Total   time.Duration
	Reused  bool
	BodyLen int
}

func main() {
	fmt.Println("=== table42 live benchmark ===")
	fmt.Println()

	authToken := os.Getenv("RESY_AUTH_TOKEN")
	if authToken == "" {
		// Try loading from token cache
		storePath := filepath.Join(os.Getenv("HOME"), ".noresi", "resy_tokens.json")
		data, err := os.ReadFile(storePath)
		if err == nil {
			var store map[string]struct {
				AuthToken string `json:"auth_token"`
			}
			json.Unmarshal(data, &store)
			for _, tok := range store {
				authToken = tok.AuthToken
				break
			}
		}
	}

	venueID := intEnvBench("RESY_VENUE_ID", 25973) // L'Artusi default
	date := os.Getenv("RESY_DATE")
	if date == "" {
		date = time.Now().AddDate(0, 0, 7).Format("2006-01-02") // 1 week out
	}
	partySize := intEnvBench("RESY_PARTY_SIZE", 2)

	findURL := fmt.Sprintf("https://api.resy.com/4/find?lat=0&long=0&day=%s&party_size=%d&venue_id=%d",
		date, partySize, venueID)

	fmt.Printf("Venue: %d | Date: %s | Party: %d\n", venueID, date, partySize)
	fmt.Printf("Auth token: %s\n", truncateBench(authToken, 20))
	fmt.Println()

	// Test 1: Cold vs Warm latency
	fmt.Println("── Test 1: Cold vs Warm latency ──")
	benchColdWarm(findURL, authToken, 5)

	// Test 2: Parallel shot spread
	fmt.Println("── Test 2: Parallel shot spread ──")
	benchParallelSpread(findURL, authToken)

	// Test 3: Byte scanner vs json.Unmarshal
	fmt.Println("── Test 3: Byte scanner vs json.Unmarshal ──")
	benchParser(findURL, authToken)

	// Test 4: Response sizes
	fmt.Println("── Test 4: Response sizes ──")
	benchResponseSize(findURL, authToken)

	// Test 5: Payload construction
	fmt.Println("── Test 5: Payload construction ──")
	benchPayloadConstruction(venueID, date, partySize)

	// Test 6: Rate limit detection
	fmt.Println("── Test 6: Rate limit detection ──")
	benchRateLimits(findURL, authToken)

	// Test 7: Full pipeline (find → details, no actual booking)
	if authToken != "" {
		fmt.Println("── Test 7: Find → Details pipeline ──")
		benchPipeline(findURL, authToken, venueID, date, partySize)
	} else {
		fmt.Println("── Test 7: Skipped (no auth token) ──")
	}

	fmt.Println()
	fmt.Println("=== benchmark complete ===")
}

func benchColdWarm(findURL, authToken string, iterations int) {
	fmt.Println("Cold (new transport each):")
	for i := 0; i < iterations; i++ {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			DisableCompression:  false,
			ForceAttemptHTTP2:   true,
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		tr := tracedRequest(client, "GET", findURL, authToken, nil)
		fmt.Printf("  [%d] DNS=%v TCP=%v TLS=%v TTFB=%v Total=%v Body=%dB\n",
			i, tr.DNS, tr.TCP, tr.TLS, tr.TTFB, tr.Total, tr.BodyLen)
	}

	// Warm: shared transport
	fmt.Println("Warm (shared transport):")
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	// Prime connection
	tracedRequest(client, "GET", findURL, authToken, nil)

	for i := 0; i < iterations; i++ {
		tr := tracedRequest(client, "GET", findURL, authToken, nil)
		fmt.Printf("  [%d] DNS=%v TCP=%v TLS=%v TTFB=%v Total=%v Reused=%v Body=%dB\n",
			i, tr.DNS, tr.TCP, tr.TLS, tr.TTFB, tr.Total, tr.Reused, tr.BodyLen)
	}
	fmt.Println()
}

func benchParallelSpread(findURL, authToken string) {
	for _, n := range []int{1, 3, 6} {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			DisableCompression:  false,
			ForceAttemptHTTP2:   true,
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		// Prime
		tracedRequest(client, "GET", findURL, authToken, nil)

		ready := make(chan struct{})
		var mu sync.Mutex
		var results []traceResult

		for i := 0; i < n; i++ {
			go func() {
				<-ready
				tr := tracedRequest(client, "GET", findURL, authToken, nil)
				mu.Lock()
				results = append(results, tr)
				mu.Unlock()
			}()
		}
		close(ready)

		// Wait for all
		for len(results) < n {
			runtime.Gosched()
		}

		var minT, maxT time.Duration
		var sum time.Duration
		reused := 0
		for i, r := range results {
			if i == 0 || r.Total < minT {
				minT = r.Total
			}
			if r.Total > maxT {
				maxT = r.Total
			}
			sum += r.Total
			if r.Reused {
				reused++
			}
		}
		avg := sum / time.Duration(n)
		fmt.Printf("  %d shots: min=%v max=%v spread=%v avg=%v reused=%d/%d\n",
			n, minT, maxT, maxT-minT, avg, reused, n)
	}
	fmt.Println()
}

func benchParser(findURL, authToken string) {
	// Fetch a real response
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	data := fetchBody(client, findURL, authToken)
	if len(data) == 0 {
		fmt.Println("  Skipped (empty response)")
		fmt.Println()
		return
	}

	iterations := 1000

	// Byte scanner
	tokenMarker := []byte(`rgs://resy/`)
	start := time.Now()
	for i := 0; i < iterations; i++ {
		pos := 0
		count := 0
		for {
			idx := bytes.Index(data[pos:], tokenMarker)
			if idx < 0 {
				break
			}
			tokenStart := pos + idx
			tokenEnd := bytes.IndexByte(data[tokenStart:], '"')
			if tokenEnd < 0 {
				break
			}
			_ = string(data[tokenStart : tokenStart+tokenEnd])
			count++
			pos = tokenStart + tokenEnd + 1
		}
	}
	byteTime := time.Since(start)

	// json.Unmarshal
	start = time.Now()
	for i := 0; i < iterations; i++ {
		var result map[string]any
		json.Unmarshal(data, &result)
	}
	jsonTime := time.Since(start)

	fmt.Printf("  Byte scanner: %v (%v/op) on %dB response\n",
		byteTime, byteTime/time.Duration(iterations), len(data))
	fmt.Printf("  json.Unmarshal: %v (%v/op) on %dB response\n",
		jsonTime, jsonTime/time.Duration(iterations), len(data))
	if byteTime > 0 {
		fmt.Printf("  Speedup: %.0fx\n", float64(jsonTime)/float64(byteTime))
	}
	fmt.Println()
}

func benchResponseSize(findURL, authToken string) {
	for i := 0; i < 3; i++ {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			DisableCompression:  true, // Get uncompressed size
			ForceAttemptHTTP2:   true,
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		uncompressed := fetchBody(client, findURL, authToken)

		transport2 := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			DisableCompression:  false, // Get compressed size
			ForceAttemptHTTP2:   true,
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		}
		client2 := &http.Client{Transport: transport2, Timeout: 10 * time.Second}
		compressed := fetchRawBody(client2, findURL, authToken)

		ratio := float64(len(compressed)) / float64(len(uncompressed)) * 100
		fmt.Printf("  [%d] Uncompressed: %dB | Compressed: %dB (%.1f%%)\n",
			i, len(uncompressed), len(compressed), ratio)
	}
	fmt.Println()
}

func benchPayloadConstruction(venueID int, date string, partySize int) {
	iterations := 10000

	// fmt.Sprintf
	start := time.Now()
	for i := 0; i < iterations; i++ {
		_ = fmt.Sprintf("https://api.resy.com/4/find?lat=0&long=0&day=%s&party_size=%d&venue_id=%d",
			date, partySize, venueID)
	}
	sprintfTime := time.Since(start)

	// Pre-built (the URL never changes per-booking)
	prebuilt := fmt.Sprintf("https://api.resy.com/4/find?lat=0&long=0&day=%s&party_size=%d&venue_id=%d",
		date, partySize, venueID)
	start = time.Now()
	for i := 0; i < iterations; i++ {
		_ = prebuilt
	}
	prebuiltTime := time.Since(start)

	// url.Values
	start = time.Now()
	for i := 0; i < iterations; i++ {
		v := url.Values{}
		v.Set("book_token", "test-token-value")
		v.Set("struct_payment_method", fmt.Sprintf(`{"id":%d}`, 12345))
		v.Set("source_id", "resy.com-venue-details")
		_ = v.Encode()
	}
	urlValuesTime := time.Since(start)

	fmt.Printf("  fmt.Sprintf:  %v (%v/op)\n", sprintfTime, sprintfTime/time.Duration(iterations))
	fmt.Printf("  Pre-built:    %v (%v/op)\n", prebuiltTime, prebuiltTime/time.Duration(iterations))
	fmt.Printf("  url.Values:   %v (%v/op)\n", urlValuesTime, urlValuesTime/time.Duration(iterations))
	fmt.Println()
}

func benchRateLimits(findURL, authToken string) {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	rateLimited := 0
	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest("GET", findURL, nil)
		req.Header.Set("Authorization", benchResyAPIKey)
		req.Header.Set("User-Agent", benchUserAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", "https://resy.com")
		req.Header.Set("Referer", "https://resy.com/")
		req.Header.Set("X-Origin", "https://resy.com")
		if authToken != "" {
			req.Header.Set("X-Resy-Auth-Token", authToken)
		}

		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start)

		if err != nil {
			fmt.Printf("  [%d] ERROR: %v\n", i, err)
			continue
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			rateLimited++
		}
		fmt.Printf("  [%d] HTTP %d (%v)\n", i, resp.StatusCode, elapsed)
	}
	fmt.Printf("  Rate limited: %d/10\n", rateLimited)
	fmt.Println()
}

func benchPipeline(findURL, authToken string, venueID int, date string, partySize int) {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	// Prime
	fetchBody(client, findURL, authToken)

	for i := 0; i < 3; i++ {
		// Step 1: Find
		t0 := time.Now()
		findData := fetchBody(client, findURL, authToken)
		findTime := time.Since(t0)

		// Parse slots — scan for rgs://resy/ token values
		tokenMarker := []byte(`rgs://resy/`)
		configToken := ""
		idx := bytes.Index(findData, tokenMarker)
		if idx >= 0 {
			tokenStart := idx
			tokenEnd := bytes.IndexByte(findData[tokenStart:], '"')
			if tokenEnd >= 0 {
				configToken = string(findData[tokenStart : tokenStart+tokenEnd])
			}
		}

		if configToken == "" {
			fmt.Printf("  [%d] Find: %v — no slots found (response %dB)\n", i, findTime, len(findData))
			continue
		}

		// Step 2: Details
		detailsURL := fmt.Sprintf("https://api.resy.com/3/details?day=%s&party_size=%d&venue_id=%d&config_id=%s",
			date, partySize, venueID, url.QueryEscape(configToken))

		t1 := time.Now()
		detailsData := fetchBodyAuth(client, detailsURL, authToken)
		detailsTime := time.Since(t1)

		// Extract book_token value
		bookTokenKey := []byte(`"value":`)
		bookTokenOuter := []byte(`"book_token"`)
		bookToken := ""
		outerIdx := bytes.Index(detailsData, bookTokenOuter)
		if outerIdx >= 0 {
			region := detailsData[outerIdx:]
			valIdx := bytes.Index(region, bookTokenKey)
			if valIdx >= 0 {
				after := region[valIdx+len(bookTokenKey):]
				// Skip whitespace
				for len(after) > 0 && (after[0] == ' ' || after[0] == '\n' || after[0] == '\r' || after[0] == '\t') {
					after = after[1:]
				}
				if len(after) > 0 && after[0] == '"' {
					after = after[1:]
					end := bytes.IndexByte(after, '"')
					if end > 0 {
						bookToken = string(after[:end])
					}
				}
			}
		}

		total := findTime + detailsTime

		// Step 3: Book (only if we have a book token — will likely fail without payment)
		bookTime := time.Duration(0)
		bookStatus := "skipped"
		if bookToken != "" {
			t2 := time.Now()
			bookReq, _ := http.NewRequest("POST", "https://api.resy.com/3/book",
				bytes.NewReader([]byte("book_token="+url.QueryEscape(bookToken)+
					"&struct_payment_method=%7B%22id%22%3A0%7D&source_id=resy.com-venue-details")))
			bookReq.Header.Set("Authorization", benchResyAPIKey)
			bookReq.Header.Set("User-Agent", benchUserAgent)
			bookReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			bookReq.Header.Set("Accept", "application/json")
			bookReq.Header.Set("Origin", "https://resy.com")
			bookReq.Header.Set("Referer", "https://resy.com/")
			bookReq.Header.Set("X-Origin", "https://resy.com")
			bookReq.Header.Set("X-Resy-Auth-Token", authToken)
			bookReq.Header.Set("X-Resy-Universal-Auth", authToken)
			bookResp, err := client.Do(bookReq)
			bookTime = time.Since(t2)
			if err != nil {
				bookStatus = fmt.Sprintf("error: %v", err)
			} else {
				bookBody, _ := readBodyBench(bookResp)
				bookResp.Body.Close()
				bookStatus = fmt.Sprintf("HTTP %d (%dB)", bookResp.StatusCode, len(bookBody))
			}
		}

		fmt.Printf("  [%d] Find: %v | Details: %v | Book: %v [%s] | Total: %v | Token: %s...\n",
			i, findTime, detailsTime, bookTime, bookStatus, total+bookTime,
			configToken[:min64(len(configToken), 50)])
	}
	fmt.Println()
}

// ── Helpers ──

func tracedRequest(client *http.Client, method, rawURL, authToken string, body []byte) traceResult {
	var result traceResult
	var dnsStart, connStart, tlsStart, gotConn time.Time
	requestStart := time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			result.DNS = time.Since(dnsStart)
		},
		ConnectStart: func(network, addr string) {
			connStart = time.Now()
		},
		ConnectDone: func(network, addr string, err error) {
			result.TCP = time.Since(connStart)
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			result.TLS = time.Since(tlsStart)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn = time.Now()
			result.Reused = info.Reused
		},
		GotFirstResponseByte: func() {
			if !gotConn.IsZero() {
				result.TTFB = time.Since(gotConn)
			}
		},
	}

	ctx := httptrace.WithClientTrace(context.Background(), trace)
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	req.Header.Set("Authorization", benchResyAPIKey)
	req.Header.Set("User-Agent", benchUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	if authToken != "" {
		req.Header.Set("X-Resy-Auth-Token", authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Total = time.Since(requestStart)
		return result
	}

	data, _ := readBodyBench(resp)
	resp.Body.Close()
	result.Total = time.Since(requestStart)
	result.BodyLen = len(data)
	return result
}

func fetchBody(client *http.Client, rawURL, authToken string) []byte {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("Authorization", benchResyAPIKey)
	req.Header.Set("User-Agent", benchUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	if authToken != "" {
		req.Header.Set("X-Resy-Auth-Token", authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	data, _ := readBodyBench(resp)
	resp.Body.Close()
	return data
}

func fetchBodyAuth(client *http.Client, rawURL, authToken string) []byte {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("Authorization", benchResyAPIKey)
	req.Header.Set("User-Agent", benchUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	if authToken != "" {
		req.Header.Set("X-Resy-Auth-Token", authToken)
		req.Header.Set("X-Resy-Universal-Auth", authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	data, _ := readBodyBench(resp)
	resp.Body.Close()
	return data
}

func fetchRawBody(client *http.Client, rawURL, authToken string) []byte {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("Authorization", benchResyAPIKey)
	req.Header.Set("User-Agent", benchUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("X-Origin", "https://resy.com")
	if authToken != "" {
		req.Header.Set("X-Resy-Auth-Token", authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	data, _ := io.ReadAll(resp.Body) // raw — don't decompress
	resp.Body.Close()
	return data
}

func readBodyBench(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return io.ReadAll(resp.Body)
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(reader)
}

func intEnvBench(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func truncateBench(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func min64(a, b int) int {
	if a < b {
		return a
	}
	return b
}
