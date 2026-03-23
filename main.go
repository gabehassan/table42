package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Semi-static API key — same for all Resy web users.
const resyAPIKey = `ResyAPI api_key="VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"`

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// Package-level vars set once at startup — zero alloc in hot path.
var (
	pinnedIP   string
	authHeader string // raw auth token value (not the "ResyAPI api_key" header)
	venueName  string // fetched once at startup for webhooks/logs
)

// ───────────────────────────── Timing Log ─────────────────────────────
// Captures timestamps in the hot path (time.Now() costs ~20ns, no alloc).
// Written to stderr and optionally to a file AFTER the critical window.

type TimingEntry struct {
	Step    string        `json:"step"`
	At      time.Time     `json:"at"`
	Elapsed time.Duration `json:"elapsed_ns"`
	Status  int           `json:"status,omitempty"`
	Size    int           `json:"size,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

type TimingLog struct {
	mu      sync.Mutex
	entries []TimingEntry
	start   time.Time
}

func newTimingLog() *TimingLog {
	return &TimingLog{start: time.Now()}
}

// record captures a timing entry. Mutex-protected for concurrent goroutine access.
func (tl *TimingLog) record(step string, status, size int, detail string) {
	now := time.Now()
	tl.mu.Lock()
	tl.entries = append(tl.entries, TimingEntry{
		Step:    step,
		At:      now,
		Elapsed: now.Sub(tl.start),
		Status:  status,
		Size:    size,
		Detail:  detail,
	})
	tl.mu.Unlock()
}

// snapshot returns a copy of entries for safe reading from other goroutines.
func (tl *TimingLog) snapshot() []TimingEntry {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	cp := make([]TimingEntry, len(tl.entries))
	copy(cp, tl.entries)
	return cp
}

// dump writes the full timing log to stderr and optionally to a file.
// Called AFTER the critical window — never in the hot path.
func (tl *TimingLog) dump() {
	logf("─── Timing Log ───")
	for _, e := range tl.entries {
		if e.Status > 0 {
			logf("  [%v] %s — HTTP %d, %dB %s", e.Elapsed.Round(time.Microsecond), e.Step, e.Status, e.Size, e.Detail)
		} else {
			logf("  [%v] %s %s", e.Elapsed.Round(time.Microsecond), e.Step, e.Detail)
		}
	}
	total := time.Since(tl.start)
	logf("  Total: %v", total.Round(time.Microsecond))

	// Always write JSON log — to RESY_LOG_FILE if set, else ~/.noresi/table42.log
	logPath := os.Getenv("RESY_LOG_FILE")
	if logPath == "" {
		logPath = filepath.Join(os.Getenv("HOME"), ".noresi", "table42.log")
	}
	os.MkdirAll(filepath.Dir(logPath), 0700)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	// Include config context so every log entry is self-contained for debugging
	entry := map[string]any{
		"timestamp":  tl.start.Format(time.RFC3339Nano),
		"venue_id":   os.Getenv("RESY_VENUE_ID"),
		"date":       os.Getenv("RESY_DATE"),
		"time":       os.Getenv("RESY_TIME"),
		"party_size": os.Getenv("RESY_PARTY_SIZE"),
		"table_type": os.Getenv("RESY_TABLE_TYPE"),
		"drop_time":  os.Getenv("RESY_DROP_TIME"),
		"shots":      os.Getenv("RESY_SHOTS"),
		"pinned_ip":  pinnedIP,
		"entries":    tl.entries,
		"total_ms":   total.Milliseconds(),
	}
	data, _ := json.Marshal(entry)
	f.Write(data)
	f.Write([]byte("\n"))
	logf("Log written to %s", logPath)
}

// Pre-compiled byte markers for scanner — allocated once.
// NOTE: Resy API returns pretty-printed JSON ("token": "..." with space).
// We search for the value prefix directly to handle both compact and pretty JSON.
var (
	tokenMarker    = []byte(`rgs://resy/`)
	bookTokenKey   = []byte(`"value":`)
	bookTokenOuter = []byte(`"book_token"`)
	resIDKey       = []byte(`"reservation_id":`)
	resyTokenKey   = []byte(`"resy_token":`)
)

type Config struct {
	Email       string
	Password    string
	AuthToken   string // RESY_AUTH_TOKEN — optional, auto-login if missing
	VenueID     int    // RESY_VENUE_ID
	Date        string // YYYY-MM-DD
	Time        string // HH:MM (24h) — preferred time (slots sorted by proximity)
	TimeRange   string // RESY_TIME_RANGE — e.g. "17:00-22:00" (accept any slot in range)
	PartySize   int    // default 2
	TableType   string // e.g. "Indoor Dining", "Bar Counter" — empty = any
	PaymentID   int    // RESY_PAYMENT_ID — required for booking
	DropTime    time.Time
	Shots       int    // parallel find shots, default 3
	MaxBook     int    // RESY_MAX_BOOK — parallel booking attempts, default 5
	Monitor     bool
	MonInterval int    // monitor poll interval in seconds
	ProxyFile   string
	OutputJSON  bool
	WebhookURL    string // RESY_WEBHOOK — Discord/Slack webhook URL
	CapSolverKey string // RESY_CAPSOLVER_KEY — CAPSolver API key (AI-based, 5-15s)
	CaptchaToken string // pre-solved captcha token (set at runtime)
}

type Slot struct {
	ConfigToken string // rgs://resy/... — passed to /3/details as config_id
	TableType   string // "Indoor Dining", "Bar Counter", etc.
	StartTime   string // "2026-03-23 12:00:00"
	TimeOnly    string // "12:00" — for matching against target
}

func main() {
	// Subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			handleSetup()
			return
		case "search":
			query := strings.Join(os.Args[2:], " ")
			handleSearch(query)
			return
		case "snipe":
			// Easy mode: ./table42 snipe "https://resy.com/cities/ny/venue?date=2026-04-01&seats=2"
			if len(os.Args) < 3 {
				fmt.Println("Usage: ./table42 snipe <resy-url>")
				fmt.Println("Example: ./table42 snipe \"https://resy.com/cities/new-york-ny/venues/lartusi-ny?date=2026-04-01&seats=2\"")
				return
			}
			handleSnipe(os.Args[2])
			return
		case "cancel":
			loadDotEnv()
			handleCancel(os.Args[2:])
			return
		case "test":
			// Send test webhook and exit
			loadDotEnv()
			webhookURL := os.Getenv("RESY_WEBHOOK")
			if webhookURL == "" {
				fatal("RESY_WEBHOOK not set.")
			}
			ips, _ := net.LookupHost("api.resy.com")
			if len(ips) > 0 {
				pinnedIP = ips[0]
			}
			if vid := intEnv("RESY_VENUE_ID", 0); vid > 0 {
				if name := fetchVenueName(vid); name != "" {
					venueName = name
				}
			}
			sendTestWebhook(webhookURL)
			// Also send a mock success and failure so you can see both
			time.Sleep(500 * time.Millisecond)
			notifyWebhook(webhookURL,
				"Booked! 2026-04-12 at 19:00",
				"**Venue:** 4 Charles Prime Rib (834)\n**Date:** 2026-04-12\n**Time:** 19:00\n**Party:** 2\n**Table:** Indoor Dining\n**Reservation:** 854026856\n**Pipeline:** 273ms",
				true)
			time.Sleep(500 * time.Millisecond)
			notifyWebhook(webhookURL,
				"Failed — 2026-04-12 at 19:00",
				"**Venue:** 4 Charles Prime Rib (834)\nAll 5 attempts failed\n**Last error:** HTTP 412: slot already taken",
				false)
			time.Sleep(1 * time.Second) // wait for goroutines
			logf("Test webhooks sent (config, success, failure).")
			return
		}
	}

	cfg := loadConfig()
	tlog = newTimingLog()
	defer tlog.dump() // always write timing log, even on fatal

	// ─── Phase 0: Auth ───
	if cfg.AuthToken == "" {
		if cfg.Email == "" || cfg.Password == "" {
			fatal("Set RESY_AUTH_TOKEN, or both RESY_EMAIL and RESY_PASSWORD.")
		}
		token, paymentID, err := getAuthToken(cfg.Email, cfg.Password)
		if err != nil {
			fatal("Auth failed: %v", err)
		}
		cfg.AuthToken = token
		if cfg.PaymentID == 0 && paymentID != 0 {
			cfg.PaymentID = paymentID
			logf("Using payment method ID %d from login response.", paymentID)
		}
	}
	authHeader = cfg.AuthToken
	initBaseHeaders() // pre-build HTTP headers once — avoids 11 Set() calls per request

	// Try to get payment ID if still missing — payment_id=0 works for
	// venues with allow_bypass_payment_method=1 (e.g., 4 Charles, L'Artusi)
	if cfg.PaymentID == 0 {
		logf("No RESY_PAYMENT_ID set, fetching from account...")
		id, err := getPaymentMethodID(cfg.AuthToken)
		if err != nil {
			logf("Warning: %v — using payment_id=0 (works for no-deposit venues)", err)
		} else {
			cfg.PaymentID = id
			logf("Using payment method ID %d", id)
		}
	}

	// ─── Phase 1: Pre-build URLs + payloads ───
	findURL := buildFindURL(cfg.VenueID, cfg.Date, cfg.PartySize)
	findBody := buildFindBody(cfg.VenueID, cfg.Date, cfg.PartySize)

	// Fetch venue name (one API call, before critical window — not in hot path)
	venueName = fmt.Sprintf("venue-%d", cfg.VenueID)
	if name := fetchVenueName(cfg.VenueID); name != "" {
		venueName = name
	}
	logf("Target: %s (%d) on %s at %s, party of %d", venueName, cfg.VenueID, cfg.Date, cfg.Time, cfg.PartySize)
	if cfg.TableType != "" {
		logf("Preferred table type: %s", cfg.TableType)
	}

	// ─── Phase 2: Build client with DNS pinning ───
	client := buildClient()

	// Send startup webhook (after DNS pinning so IP shows in webhook)
	sendTestWebhook(cfg.WebhookURL)

	// ─── Phase 3: Mode selection ───
	var slots []Slot
	skipFind := false

	if cfg.Monitor {
		// Monitor mode: poll with proxies until slots appear
		pool := loadProxies(cfg.ProxyFile)
		monClient := buildMonitorClient(pool)
		monSlots := monitorForSlots(monClient, cfg.VenueID, cfg.Date, cfg.Time, cfg.PartySize, cfg.MonInterval)
		logf("Monitor found %d slots — switching to sniper mode.", len(monSlots))

		warmConnections(client, cfg.Shots, findURL, findBody)
		initBookPayload(cfg.PaymentID, cfg.CaptchaToken)
		runtime.GC()
		debug.SetGCPercent(-1)

		slots = monSlots
		skipFind = true

	} else if !cfg.DropTime.IsZero() {
		// Drop-time mode: captcha → sleep → warm → pre-poll → spin-wait → fire
		//
		// Timeline:
		//   T-90s  Pre-solve captcha (if RESY_CAPSOLVER_KEY set)
		//   T-30s  Warm connections
		//   T-10s  Pre-poll for early drops
		//   T-0s   Spin-wait fires with 33ns precision

		// Pre-solve captcha at T-60s (tokens valid ~120s, solving takes 10-30s)
		// Only runs in drop-time mode — no point pre-solving for immediate bookings
		// since most venues don't require captcha.
		captchaLead := intEnv("RESY_CAPTCHA_LEAD", 60) // seconds before drop to solve
		if cfg.CapSolverKey != "" {
			captchaDur := time.Until(cfg.DropTime) - time.Duration(captchaLead)*time.Second
			if captchaDur > 0 {
				logf("Sleeping until T-%ds for captcha pre-solve...", captchaLead)
				time.Sleep(captchaDur)
			}
			preSolveCaptcha(&cfg)
		}

		sleepDur := time.Until(cfg.DropTime) - 30*time.Second
		if sleepDur > 0 {
			logf("Sleeping until T-30s (%s)...", cfg.DropTime.Add(-30*time.Second).Format("15:04:05"))
			time.Sleep(sleepDur)
		}

		warmConnections(client, cfg.Shots, findURL, findBody)

		// Pre-build book payload AFTER captcha solve (includes token if available)
		initBookPayload(cfg.PaymentID, cfg.CaptchaToken)

		runtime.GC()
		debug.SetGCPercent(-1)

		// Pre-poll phase: rapid-fire find requests from T-10s to T
		// If slots appear early, skip the spin-wait and book immediately
		prePollStart := cfg.DropTime.Add(-10 * time.Second)
		if time.Now().Before(prePollStart) {
			logf("Waiting until T-10s for pre-poll (%s)...", prePollStart.Format("15:04:05"))
			time.Sleep(time.Until(prePollStart))
		}

		logf("Pre-polling from T-10s to T (%s → %s)...", time.Now().Format("15:04:05.000"), cfg.DropTime.Format("15:04:05.000"))
		tlog.record("pre-poll-start", 0, 0, "")

		pollInterval := 150 * time.Millisecond
		for time.Now().Before(cfg.DropTime) {
			data, status, err := doRequest(context.Background(), client, "POST", "https://api.resy.com/4/find", findBody, "application/json")
			if err == nil && status == 200 {
				earlySlots := parseSlots(data, "")
				if len(earlySlots) > 0 {
					logf("EARLY DROP! Found %d slots at T-%v", len(earlySlots), time.Until(cfg.DropTime).Round(time.Millisecond))
					tlog.record("early-drop", 0, len(earlySlots), fmt.Sprintf("T-%v", time.Until(cfg.DropTime).Round(time.Millisecond)))
					slots = earlySlots
					skipFind = true
					break
				}
			} else if status == 500 {
				// Back off on 500 to avoid deepening a rate limit
				pollInterval = 500 * time.Millisecond
			}
			time.Sleep(pollInterval)
		}

		if !skipFind {
			// No early drop — spin-wait to exact drop time with nanosecond precision
			spinUntil(cfg.DropTime)
			drift := time.Since(cfg.DropTime)
			logf("FIRING! (drift: %v)", drift)
			tlog.record("spin-wait-done", 0, 0, fmt.Sprintf("drift=%v", drift))

			// Fire parallel shots, but if no slots found, keep polling for up to
			// 30 seconds AFTER drop time. Resy may drop a few seconds late.
			slots = fireAvailabilityShots(client, cfg.Shots, findURL, findBody, "")
			if len(slots) == 0 {
				logf("No slots at T-0 — continuing to poll for 30s...")
				deadline := cfg.DropTime.Add(30 * time.Second)
				for time.Now().Before(deadline) {
					data, status, err := doRequest(context.Background(), client, "POST", "https://api.resy.com/4/find", findBody, "application/json")
					if err == nil && status == 200 {
						lateSlots := parseSlots(data, "")
						if len(lateSlots) > 0 {
							logf("LATE DROP! Found %d slots at T+%v", len(lateSlots), time.Since(cfg.DropTime).Round(time.Millisecond))
							tlog.record("late-drop", 0, len(lateSlots), fmt.Sprintf("T+%v", time.Since(cfg.DropTime).Round(time.Millisecond)))
							slots = lateSlots
							break
						}
					}
					time.Sleep(150 * time.Millisecond)
				}
				skipFind = true // already have slots (or still empty)
			} else {
				skipFind = true
			}
		}

	} else {
		// Immediate mode
		warmConnections(client, cfg.Shots, findURL, findBody)
		initBookPayload(cfg.PaymentID, cfg.CaptchaToken)
		runtime.GC()
		debug.SetGCPercent(-1)
		logf("Firing immediately...")
		tlog.record("immediate-fire", 0, 0, "")
	}

	// ─── Phase 4+5: Find slots and book ───
	searchTime := cfg.Time
	if cfg.TimeRange != "" {
		searchTime = "" // accept all, filter in bookSlots
	}
	if !skipFind {
		slots = fireAvailabilityShots(client, cfg.Shots, findURL, findBody, searchTime)
	}
	bookSlots(client, cfg, slots)
}

// ───────────────────────────── Booking Pipeline ─────────────────────────────
// Shared by main() and handleSnipe(). Filters, sorts, and books from a slice
// of available slots. This is the hot path after slot discovery.

type bookResult struct {
	reservationID string
	resyToken     string
	slot          Slot
	err           error
}

func bookSlots(client *http.Client, cfg Config, slots []Slot) {
	// Apply time range filter
	if cfg.TimeRange != "" && len(slots) > 0 {
		rangeStart, rangeEnd := parseTimeRange(cfg.TimeRange)
		if rangeStart >= 0 && rangeEnd >= 0 {
			total := len(slots)
			var inRange []Slot
			for _, s := range slots {
				m := timeToMinutes(s.TimeOnly)
				if m >= rangeStart && m <= rangeEnd {
					inRange = append(inRange, s)
				}
			}
			if len(inRange) > 0 {
				slots = inRange
			}
			logf("Time range %s: %d/%d slots in window", cfg.TimeRange, len(inRange), total)
		}
	}

	if len(slots) == 0 {
		debug.SetGCPercent(100)
		tlog.record("no-slots-found", 0, 0, "")
		notifyWebhook(cfg.WebhookURL,
			fmt.Sprintf("No slots — %s at %s", venueName, cfg.Time),
			fmt.Sprintf("**Venue:** %s (%d)\nNo matching slots found after %d shots",
				venueName, cfg.VenueID, cfg.Shots),
			false)
		fatal("No matching slots found.")
	}

	// Sort by proximity to preferred time
	if cfg.Time != "" {
		sortSlotsByProximity(slots, timeToMinutes(cfg.Time))
	}

	tlog.record("slots-found", 0, len(slots), fmt.Sprintf("%d slots", len(slots)))
	logf("Found %d matching slots:", len(slots))
	for i, s := range slots {
		preview := s.ConfigToken
		if len(preview) > 60 {
			preview = preview[:60] + "..."
		}
		logf("  [%d] %s — %s (%s)", i, s.TimeOnly, s.TableType, preview)
	}

	// Filter by table type preference
	if cfg.TableType != "" {
		var preferred []Slot
		for _, s := range slots {
			if strings.EqualFold(s.TableType, cfg.TableType) {
				preferred = append(preferred, s)
			}
		}
		if len(preferred) > 0 {
			slots = preferred
			logf("Filtered to %d slots matching table type %q", len(slots), cfg.TableType)
		} else {
			logf("Warning: no slots match table type %q, using all %d slots", cfg.TableType, len(slots))
		}
	}

	// Parallel details→book across top N slots
	n := cfg.MaxBook
	if len(slots) < n {
		n = len(slots)
	}
	logf("Attempting %d parallel bookings (of %d available slots)", n, len(slots))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan bookResult, n)

	for _, s := range slots[:n] {
		go func(slot Slot) {
			bt, err := getBookToken(ctx, client, cfg, slot)
			if err != nil {
				results <- bookResult{err: fmt.Errorf("details for %s: %w", slot.TimeOnly, err), slot: slot}
				return
			}
			rid, rt, err := bookReservation(ctx, client, cfg, bt)
			if err != nil {
				results <- bookResult{err: fmt.Errorf("book %s: %w", slot.TimeOnly, err), slot: slot}
				return
			}
			results <- bookResult{reservationID: rid, resyToken: rt, slot: slot}
		}(s)
	}

	var lastErr error
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			logf("Booking attempt failed: %v", r.err)
			tlog.record("book-failed", 0, 0, r.err.Error())
			lastErr = r.err
			continue
		}

		// SUCCESS
		cancel()
		debug.SetGCPercent(100)
		tlog.record("BOOKED", 201, 0, fmt.Sprintf("res=%s slot=%s/%s", r.reservationID, r.slot.TimeOnly, r.slot.TableType))
		logf("BOOKED! Reservation %s — %s at %s (%s)", r.reservationID, cfg.Date, r.slot.TimeOnly, r.slot.TableType)

		// Webhook with account name from token cache (zero API cost)
		accountName := cfg.Email
		if store := loadTokenStore(); len(store) > 0 {
			for _, tok := range store {
				if tok.FirstName != "" {
					accountName = tok.FirstName + " " + tok.LastName
					break
				}
			}
		}

		notifyWebhook(cfg.WebhookURL,
			fmt.Sprintf("Booked! %s at %s", venueName, r.slot.TimeOnly),
			fmt.Sprintf("**Venue:** %s (%d)\n**Date:** %s\n**Time:** %s\n**Party:** %d\n**Table:** %s\n**Reservation:** %s\n**Account:** %s\n**Pipeline:** %v",
				venueName, cfg.VenueID, cfg.Date, r.slot.TimeOnly, cfg.PartySize, r.slot.TableType,
				r.reservationID, accountName, time.Since(tlog.start).Round(time.Millisecond)),
			true)

		saveBooking(Booking{
			VenueID: cfg.VenueID, VenueName: venueName,
			ReservationID: r.reservationID, ResyToken: r.resyToken,
			DateTime: r.slot.StartTime, PartySize: cfg.PartySize,
			TableType: r.slot.TableType, BookedAt: time.Now(),
		})

		if cfg.OutputJSON {
			out, _ := json.Marshal(map[string]any{
				"status": "booked", "reservation_id": r.reservationID,
				"venue_id": cfg.VenueID, "date": cfg.Date, "time": r.slot.TimeOnly,
				"party_size": cfg.PartySize, "table_type": r.slot.TableType,
			})
			fmt.Println(string(out))
		}

		webhookWg.Wait()
		return
	}

	debug.SetGCPercent(100)
	notifyWebhook(cfg.WebhookURL,
		fmt.Sprintf("Failed — %s at %s", venueName, cfg.Time),
		fmt.Sprintf("**Venue:** %s (%d)\nAll %d attempts failed\n**Last error:** %v",
			venueName, cfg.VenueID, n, lastErr),
		false)
	fatal("All %d booking attempts failed. Last error: %v", n, lastErr)
}

// ───────────────────────────── HTTP Client ─────────────────────────────

func buildClient() *http.Client {
	// DNS pinning — resolve once, bypass DNS jitter in critical path
	ips, err := net.LookupHost("api.resy.com")
	if err == nil && len(ips) > 0 {
		pinnedIP = ips[0]
		logf("DNS pinned api.resy.com → %s", pinnedIP)
	}

	transport := &http.Transport{
		DialContext:           pinnedDialer(pinnedIP),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100, // default 2 is a TRAP — causes reconnects
		MaxConnsPerHost:       100,
		IdleConnTimeout:       120 * time.Second,
		ExpectContinueTimeout: 0,     // skip 100-continue roundtrip on POSTs
		DisableCompression:    false, // KEEP gzip — saves wire time on large responses
		ForceAttemptHTTP2:     true,  // Resy API supports HTTP/2 (unlike OpenTable mobile)
		TLSClientConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}

	// Cookie jar — accept and resend Imperva cookies.
	// api.resy.com doesn't REQUIRE cookies (tested: 20/20 pass without),
	// but sending them back looks more legitimate and avoids future enforcement.
	jar, _ := cookiejar.New(nil)

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ───────────────────────────── Request helpers ─────────────────────────────

// Package-level timing log — set in main(), used throughout hot path.
var tlog *TimingLog

func doRequest(ctx context.Context, client *http.Client, method, rawURL string, body []byte, contentType string) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	setResyHeaders(req)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if tlog != nil {
			tlog.record(method+" "+rawURL, 0, 0, "error: "+err.Error())
		}
		return nil, 0, err
	}

	data, err := readCompressedBody(resp)
	resp.Body.Close()
	elapsed := time.Since(t0)

	// Log timing + response details — time.Now() is ~20ns, negligible in hot path.
	// On error responses, log the body so we can debug drop-day failures.
	if tlog != nil {
		endpoint := rawURL
		if i := strings.Index(rawURL, "?"); i > 0 {
			endpoint = rawURL[:i]
		}
		detail := fmt.Sprintf("rtt=%v", elapsed.Round(time.Microsecond))
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			// Log error response body (truncated) for debugging
			detail += " body=" + truncate(data, 500)
		}
		tlog.record(method+" "+endpoint, resp.StatusCode, len(data), detail)
	}

	return data, resp.StatusCode, err
}

func readCompressedBody(resp *http.Response) ([]byte, error) {
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

// ───────────────────────────── URL builders ─────────────────────────────

// fetchVenueName gets the restaurant name from the config API.
// Called once at startup — not in the hot path.
func fetchVenueName(venueID int) string {
	u := fmt.Sprintf("https://api.resy.com/2/config?venue_id=%d", venueID)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", resyAPIKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	data, _ := io.ReadAll(resp.Body)
	// The venue name is nested: {"venue":{"name":"4 Charles Prime Rib",...}}
	// Find "venue" first, then extract "name" from within that section
	venueIdx := bytes.Index(data, []byte(`"venue"`))
	if venueIdx < 0 {
		return ""
	}
	return extractJSONValue(data[venueIdx:], []byte(`"name":`))
}

// findURL for GET (used for warming + pre-poll + fallback)
func buildFindURL(venueID int, date string, partySize int) string {
	return fmt.Sprintf("https://api.resy.com/4/find?lat=0&long=0&day=%s&party_size=%d&venue_id=%d",
		date, partySize, venueID)
}

// findBody for POST (matches resy.com web app — less likely to be rate-limited)
func buildFindBody(venueID int, date string, partySize int) []byte {
	return []byte(fmt.Sprintf(`{"lat":0,"long":0,"day":"%s","party_size":%d,"venue_id":%d}`,
		date, partySize, venueID))
}

// detailsURL for GET fallback
func buildDetailsURL(venueID int, date string, partySize int, authToken, configToken string) string {
	return fmt.Sprintf("https://api.resy.com/3/details?day=%s&party_size=%d&venue_id=%d&config_id=%s",
		date, partySize, venueID, url.QueryEscape(configToken))
}

// detailsBody for POST (matches resy.com web app)
func buildDetailsBody(venueID int, date string, partySize int, configToken string) []byte {
	return []byte(fmt.Sprintf(`{"day":"%s","party_size":%d,"venue_id":%d,"config_id":"%s"}`,
		date, partySize, venueID, configToken))
}

// ───────────────────────────── Parallel find ─────────────────────────────

func fireAvailabilityShots(client *http.Client, n int, findURL string, findBody []byte, targetTime string) []Slot {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstResult := make(chan []Slot, 1)
	ready := make(chan struct{})

	for i := 0; i < n; i++ {
		go func(id int) {
			<-ready // ALL goroutines unblock simultaneously on close(ready)

			// Use POST with JSON body (matches resy.com web app).
			// Fall back to GET if POST returns 500 (some endpoints may still prefer GET).
			data, status, err := doRequest(ctx, client, "POST", "https://api.resy.com/4/find", findBody, "application/json")
			if err != nil || status == 500 || status == 405 {
				// Fallback to GET
				data, status, err = doRequest(ctx, client, "GET", findURL, nil, "")
			}
			if err != nil {
				logf("Shot %d: request error: %v", id, err)
				return
			}
			if status != 200 {
				logf("Shot %d: HTTP %d", id, status)
				return
			}

			slots := parseSlots(data, targetTime)
			if len(slots) > 0 {
				select {
				case firstResult <- slots:
					cancel() // abort other in-flight shots
				default:
				}
			} else {
				logf("Shot %d: no matching slots", id)
			}
		}(i)
	}

	close(ready) // BROADCAST — all goroutines fire simultaneously

	select {
	case slots := <-firstResult:
		return slots
	case <-time.After(10 * time.Second):
		cancel()
		return nil
	}
}

// ───────────────────────────── Byte-scan slot parser ─────────────────────────────
// Parses the /4/find response using byte scanning — 1000x faster than json.Unmarshal.
//
// Response structure (from live capture):
//   {"results":{"venues":[{"slots":[
//     {"config":{"token":"rgs://resy/25973/3996228/3/2026-03-23/2026-03-23/12:00:00/2/Bar Counter",
//                "type":"Bar Counter"},
//      "date":{"start":"2026-03-23 12:00:00","end":"2026-03-23 13:15:00"}},
//     ...
//   ]}]}}
//
// Config token format: rgs://resy/{venue_id}/{shift_id}/{svc_type}/{date}/{date}/{HH:MM:SS}/{party_size}/{table_type}
// Time is at index 6 when split by '/'.

func parseSlots(data []byte, targetTime string) []Slot {
	var slots []Slot
	targetPrefix := "/" + targetTime + ":"  // e.g. "/19:00:" matches "/19:00:00/"
	typeKey := []byte(`"type":`)
	startKey := []byte(`"start":`)

	pos := 0
	for {
		// Find next config token value (rgs://resy/...)
		idx := bytes.Index(data[pos:], tokenMarker)
		if idx < 0 {
			break
		}
		tokenStart := pos + idx

		// Find end of token string (closing quote)
		tokenEnd := bytes.IndexByte(data[tokenStart:], '"')
		if tokenEnd < 0 {
			break
		}
		configToken := string(data[tokenStart : tokenStart+tokenEnd])

		// Extract table type from nearby "type": "..." (within 300 bytes after token)
		regionEnd := tokenStart + tokenEnd + 300
		if regionEnd > len(data) {
			regionEnd = len(data)
		}
		region := data[tokenStart+tokenEnd : regionEnd]
		tableType := extractJSONValue(region, typeKey)

		// Extract start time from nearby "start": "..." (within 600 bytes after token)
		regionEnd2 := tokenStart + tokenEnd + 600
		if regionEnd2 > len(data) {
			regionEnd2 = len(data)
		}
		region2 := data[tokenStart+tokenEnd : regionEnd2]
		startTime := extractJSONValue(region2, startKey)

		// Extract time-only from the config token (faster than parsing startTime)
		timeOnly := extractTimeFromToken(configToken)

		pos = tokenStart + tokenEnd + 1

		// If no target time specified, accept all slots
		if targetTime == "" {
			slots = append(slots, Slot{
				ConfigToken: configToken,
				TableType:   tableType,
				StartTime:   startTime,
				TimeOnly:    timeOnly,
			})
			continue
		}

		// Check if token contains target time (e.g., "/19:00:" in the token path)
		if strings.Contains(configToken, targetPrefix) {
			slots = append(slots, Slot{
				ConfigToken: configToken,
				TableType:   tableType,
				StartTime:   startTime,
				TimeOnly:    timeOnly,
			})
		}
	}

	// If exact match found none, try fuzzy: find slots within ±60 minutes
	if len(slots) == 0 && targetTime != "" {
		slots = parseSlotsWindow(data, targetTime, 60)
	}

	return slots
}

// parseSlotsWindow finds all slots within ±windowMinutes of targetTime.
func parseSlotsWindow(data []byte, targetTime string, windowMinutes int) []Slot {
	targetMinutes := timeToMinutes(targetTime)
	if targetMinutes < 0 {
		return nil
	}

	var slots []Slot
	typeKey := []byte(`"type":`)
	startKey := []byte(`"start":`)

	pos := 0
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
		configToken := string(data[tokenStart : tokenStart+tokenEnd])

		regionEnd := tokenStart + tokenEnd + 300
		if regionEnd > len(data) {
			regionEnd = len(data)
		}
		tableType := extractJSONValue(data[tokenStart+tokenEnd:regionEnd], typeKey)

		regionEnd2 := tokenStart + tokenEnd + 600
		if regionEnd2 > len(data) {
			regionEnd2 = len(data)
		}
		startTime := extractJSONValue(data[tokenStart+tokenEnd:regionEnd2], startKey)

		timeOnly := extractTimeFromToken(configToken)
		slotMinutes := timeToMinutes(timeOnly)

		pos = tokenStart + tokenEnd + 1

		if slotMinutes >= 0 {
			diff := slotMinutes - targetMinutes
			if diff < 0 {
				diff = -diff
			}
			if diff <= windowMinutes {
				slots = append(slots, Slot{
					ConfigToken: configToken,
					TableType:   tableType,
					StartTime:   startTime,
					TimeOnly:    timeOnly,
				})
			}
		}
	}

	return slots
}

// extractTimeFromToken gets "HH:MM" from config token.
// Format: rgs://resy/{venue}/{shift}/{svc}/{date}/{date}/{HH:MM:SS}/{party}/{type}
func extractTimeFromToken(token string) string {
	parts := strings.Split(token, "/")
	if len(parts) >= 9 {
		timePart := parts[8] // HH:MM:SS
		if len(timePart) >= 5 {
			return timePart[:5] // HH:MM
		}
	}
	return ""
}

// sortSlotsByProximity sorts slots by distance from preferredMinutes (closest first).
func sortSlotsByProximity(slots []Slot, preferredMinutes int) {
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0; j-- {
			di := timeToMinutes(slots[j].TimeOnly) - preferredMinutes
			dj := timeToMinutes(slots[j-1].TimeOnly) - preferredMinutes
			if di < 0 {
				di = -di
			}
			if dj < 0 {
				dj = -dj
			}
			if di < dj {
				slots[j], slots[j-1] = slots[j-1], slots[j]
			} else {
				break
			}
		}
	}
}

// parseTimeRange parses "HH:MM-HH:MM" into start/end minutes.
func parseTimeRange(r string) (int, int) {
	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	return timeToMinutes(strings.TrimSpace(parts[0])), timeToMinutes(strings.TrimSpace(parts[1]))
}

func timeToMinutes(t string) int {
	if len(t) < 5 || t[2] != ':' {
		return -1
	}
	h, err1 := strconv.Atoi(t[:2])
	m, err2 := strconv.Atoi(t[3:5])
	if err1 != nil || err2 != nil {
		return -1
	}
	return h*60 + m
}

// ───────────────────────────── Details + Book ─────────────────────────────

// getBookToken calls GET /3/details to get the book_token for a slot.
func getBookToken(ctx context.Context, client *http.Client, cfg Config, slot Slot) (string, error) {
	// Use POST with JSON body (matches resy.com web app), GET as fallback.
	detailsBody := buildDetailsBody(cfg.VenueID, cfg.Date, cfg.PartySize, slot.ConfigToken)
	data, status, err := doRequest(ctx, client, "POST", "https://api.resy.com/3/details", detailsBody, "application/json")
	if err != nil || status == 500 || status == 405 {
		// Fallback to GET
		detailsURL := buildDetailsURL(cfg.VenueID, cfg.Date, cfg.PartySize, cfg.AuthToken, slot.ConfigToken)
		data, status, err = doRequest(ctx, client, "GET", detailsURL, nil, "")
	}
	if err != nil {
		return "", err
	}
	if status != 200 && status != 201 {
		return "", fmt.Errorf("HTTP %d: %s", status, truncate(data, 200))
	}

	// Byte-scan: find "book_token" then extract "value" from the object after it
	outerIdx := bytes.Index(data, bookTokenOuter)
	if outerIdx < 0 {
		return "", fmt.Errorf("no book_token in details response")
	}

	// Search for "value": after "book_token" — handles both compact and pretty JSON
	region := data[outerIdx:]
	token := extractJSONValue(region, bookTokenKey)
	if token == "" {
		return "", fmt.Errorf("no book_token value in details response")
	}

	return token, nil
}

// bookReservation calls POST /3/book with form-encoded body.
// Returns (reservationID, resyToken, error).
// bookPayloadSuffix is pre-built at startup — the static part of the book payload.
// Only book_token changes per request. Avoids url.Values + Encode() in hot path.
var bookPayloadSuffix string

func initBookPayload(paymentID int, captchaToken string) {
	bookPayloadSuffix = "&struct_payment_method=" + url.QueryEscape(fmt.Sprintf(`{"id":%d}`, paymentID)) +
		"&source_id=resy.com-venue-details"
	if captchaToken != "" {
		bookPayloadSuffix += "&captcha_token=" + url.QueryEscape(captchaToken)
	}
}

func bookReservation(ctx context.Context, client *http.Client, cfg Config, bookToken string) (string, string, error) {
	// Pre-built payload — only book_token is dynamic. ~200ns vs ~800ns for url.Values.
	body := []byte("book_token=" + url.QueryEscape(bookToken) + bookPayloadSuffix)

	data, status, err := doRequest(ctx, client, "POST", "https://api.resy.com/3/book", body, "application/x-www-form-urlencoded")
	if err != nil {
		return "", "", err
	}

	// NOTE: GET /3/book captcha bypass was patched (405 Method Not Allowed).
	// Tested 2026-03-22: GET → 405, POST without captcha_token → 201.
	// POST works without captcha on tested venues. If captcha is ever enforced,
	// will need CAPSolver integration ($0.80/1K solves).

	if status != 200 && status != 201 {
		return "", "", fmt.Errorf("HTTP %d: %s", status, truncate(data, 300))
	}

	// Extract reservation_id (may be number or string)
	resID := ""
	resIDIdx := bytes.Index(data, resIDKey)
	if resIDIdx >= 0 {
		after := data[resIDIdx+len(resIDKey):]
		// Skip whitespace
		for len(after) > 0 && (after[0] == ' ' || after[0] == '\t') {
			after = after[1:]
		}
		if len(after) > 0 {
			if after[0] == '"' {
				// String value
				resID = extractJSONStringBytes(after, []byte(`"`), false)
			} else {
				// Number value
				end := 0
				for end < len(after) && after[end] >= '0' && after[end] <= '9' {
					end++
				}
				resID = string(after[:end])
			}
		}
	}

	resyToken := extractJSONValue(data, resyTokenKey)

	if resID == "" {
		return "", "", fmt.Errorf("no reservation_id in book response: %s", truncate(data, 300))
	}

	return resID, resyToken, nil
}

// ───────────────────────────── Byte scanner helpers ─────────────────────────────

// extractJSONValue finds `key` (e.g. `"type":`) in data and returns the string value,
// handling both compact ("key":"val") and pretty-printed ("key": "val") JSON.
func extractJSONValue(data []byte, key []byte) string {
	idx := bytes.Index(data, key)
	if idx < 0 {
		return ""
	}
	start := idx + len(key)
	// Skip whitespace and opening quote
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	if start >= len(data) || data[start] != '"' {
		return ""
	}
	start++ // skip opening quote
	end := bytes.IndexByte(data[start:], '"')
	if end < 0 {
		return ""
	}
	return string(data[start : start+end])
}

// extractJSONStringBytes finds `key` in data and returns the string value after it (up to closing `"`).
func extractJSONStringBytes(data []byte, key []byte, backward bool) string {
	var idx int
	if backward {
		idx = bytes.LastIndex(data, key)
	} else {
		idx = bytes.Index(data, key)
	}
	if idx < 0 {
		return ""
	}
	start := idx + len(key)
	if start >= len(data) {
		return ""
	}
	end := bytes.IndexByte(data[start:], '"')
	if end < 0 {
		return ""
	}
	return string(data[start : start+end])
}

// extractJSONNumber finds `key` in data and returns the numeric value after it.
func extractJSONNumber(data []byte, key []byte) int {
	idx := bytes.Index(data, key)
	if idx < 0 {
		return 0
	}
	start := idx + len(key)
	// Skip whitespace
	for start < len(data) && (data[start] == ' ' || data[start] == '\t') {
		start++
	}
	end := start
	for end < len(data) && data[end] >= '0' && data[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	n, _ := strconv.Atoi(string(data[start:end]))
	return n
}

// ───────────────────────────── Spin-wait ─────────────────────────────

func spinUntil(target time.Time) {
	// Stage 1: coarse OS sleep — frees CPU, ~1ms precision
	if d := time.Until(target) - 2*time.Millisecond; d > 0 {
		time.Sleep(d)
	}
	// Stage 2: fine spin — pins goroutine to OS thread for 33ns precision
	runtime.LockOSThread()
	for time.Now().Before(target) {
		// tight busy-wait loop
	}
	runtime.UnlockOSThread()
}

// ───────────────────────────── Config ─────────────────────────────

func loadConfig() Config {
	loadDotEnv()

	cfg := Config{
		Email:       os.Getenv("RESY_EMAIL"),
		Password:    os.Getenv("RESY_PASSWORD"),
		AuthToken:   os.Getenv("RESY_AUTH_TOKEN"),
		VenueID:     intEnv("RESY_VENUE_ID", 0),
		Date:        os.Getenv("RESY_DATE"),
		Time:        os.Getenv("RESY_TIME"),
		PartySize:   intEnv("RESY_PARTY_SIZE", 2),
		TableType:   os.Getenv("RESY_TABLE_TYPE"),
		PaymentID:   intEnv("RESY_PAYMENT_ID", 0),
		Shots:       intEnv("RESY_SHOTS", 3),
		MaxBook:     intEnv("RESY_MAX_BOOK", 5),
		TimeRange:   os.Getenv("RESY_TIME_RANGE"),
		Monitor:     os.Getenv("RESY_MONITOR") == "true",
		MonInterval: intEnv("RESY_MONITOR_INTERVAL", 30),
		ProxyFile:   os.Getenv("RESY_PROXY_FILE"),
		OutputJSON:  os.Getenv("RESY_OUTPUT") == "json",
		WebhookURL:   os.Getenv("RESY_WEBHOOK"),
		CapSolverKey: os.Getenv("RESY_CAPSOLVER_KEY"),
	}

	if dt := os.Getenv("RESY_DROP_TIME"); dt != "" {
		t, err := time.Parse(time.RFC3339, dt)
		if err != nil {
			fatal("Invalid RESY_DROP_TIME (expected RFC3339): %v", err)
		}
		cfg.DropTime = t
	}

	if cfg.VenueID == 0 {
		fatal("RESY_VENUE_ID is required.")
	}
	if cfg.Date == "" {
		fatal("RESY_DATE is required (YYYY-MM-DD).")
	}

	return cfg
}

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		// Only set from .env if not already in environment.
		// os.Getenv returns "" for both unset AND set-to-empty,
		// so use os.LookupEnv to distinguish.
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func intEnv(key string, fallback int) int {
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

// ───────────────────────────── Logging ─────────────────────────────

func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(os.Stderr, "[table42 "+ts+"] "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[table42] FATAL: "+format+"\n", args...)
	webhookWg.Wait() // ensure webhooks send before exit
	os.Exit(1)
}

func truncate(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "..."
}

// ───────────────────────────── Webhook ─────────────────────────────
// Sends notifications to Discord/Slack webhooks. Called AFTER the critical
// window — never blocks the hot path.

// webhookWg tracks in-flight webhook goroutines so we can wait before exit.
var webhookWg sync.WaitGroup

func notifyWebhook(webhookURL, title, message string, success bool) {
	if webhookURL == "" {
		return
	}

	webhookWg.Add(1)
	go func() {
		defer webhookWg.Done()
		var payload []byte

		if strings.Contains(webhookURL, "discord.com") {
			// Discord rich embed
			color := 0xFF4444 // red for failure
			icon := "\u274c"  // ❌
			if success {
				color = 0x00CC66 // green for success
				icon = "\u2705"  // ✅
			}

			// Build timing field from tlog if available
			timingStr := ""
			if tlog != nil {
				for _, e := range tlog.snapshot() {
					if e.Status > 0 {
						timingStr += fmt.Sprintf("`[%v]` %s → **%d** (%dB)\n",
							e.Elapsed.Round(time.Millisecond), e.Step, e.Status, e.Size)
					} else if e.Detail != "" {
						timingStr += fmt.Sprintf("`[%v]` %s — %s\n",
							e.Elapsed.Round(time.Millisecond), e.Step, e.Detail)
					}
				}
			}

			fields := []map[string]any{}
			if timingStr != "" {
				fields = append(fields, map[string]any{
					"name":   "Pipeline Timeline",
					"value":  timingStr,
					"inline": false,
				})
			}

			payload, _ = json.Marshal(map[string]any{
				"embeds": []map[string]any{{
					"title":       icon + " " + title,
					"description": message,
					"color":       color,
					"fields":      fields,
					"timestamp":   time.Now().Format(time.RFC3339),
					"footer": map[string]any{
						"text": fmt.Sprintf("table42 • %s", pinnedIP),
					},
				}},
			})
		} else {
			// Slack / generic webhook format
			payload, _ = json.Marshal(map[string]string{
				"text": fmt.Sprintf("*%s*\n%s", title, message),
			})
		}

		req, _ := http.NewRequest("POST", webhookURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			logf("Webhook error: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// sendTestWebhook sends a test notification to verify webhook config.
func sendTestWebhook(webhookURL string) {
	if webhookURL == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"embeds": []map[string]any{{
			"title":       "\U0001f4e1 table42 — Webhook Test",
			"description": "Connection verified. Bot is ready.",
			"color":       0x5865F2, // Discord blurple
			"fields": []map[string]any{
				{"name": "Venue", "value": func() string { if venueName != "" { return venueName }; return os.Getenv("RESY_VENUE_ID") }(), "inline": true},
				{"name": "Date", "value": os.Getenv("RESY_DATE"), "inline": true},
				{"name": "Time", "value": os.Getenv("RESY_TIME"), "inline": true},
				{"name": "Range", "value": os.Getenv("RESY_TIME_RANGE"), "inline": true},
				{"name": "Drop", "value": os.Getenv("RESY_DROP_TIME"), "inline": true},
				{"name": "Shots", "value": fmt.Sprintf("%s find / %s book", os.Getenv("RESY_SHOTS"), os.Getenv("RESY_MAX_BOOK")), "inline": true},
				{"name": "DNS", "value": fmt.Sprintf("`%s`", pinnedIP), "inline": true},
				{"name": "Table Type", "value": os.Getenv("RESY_TABLE_TYPE"), "inline": true},
				{"name": "Party Size", "value": os.Getenv("RESY_PARTY_SIZE"), "inline": true},
				{"name": "Account", "value": func() string {
					if n := os.Getenv("RESY_ACCOUNT_NAME"); n != "" { return n }
					if e := os.Getenv("RESY_EMAIL"); e != "" { return e }
					for _, tok := range loadTokenStore() {
						if tok.FirstName != "" { return tok.FirstName + " " + tok.LastName }
						if tok.Email != "" { return tok.Email }
					}
					return "token-auth"
				}(), "inline": true},
				{"name": "Captcha", "value": func() string { if os.Getenv("RESY_CAPSOLVER_KEY") != "" { return "CAPSolver ready" }; return "disabled" }(), "inline": true},
			},
			"timestamp": time.Now().Format(time.RFC3339),
			"footer": map[string]any{
				"text": "table42 • ready to snipe",
			},
		}},
	})

	req, _ := http.NewRequest("POST", webhookURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logf("Test webhook error: %v", err)
		return
	}
	resp.Body.Close()
	logf("Test webhook sent.")
}
