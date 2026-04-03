# table42

A high-performance Resy reservation sniper written in Go. Books competitive restaurant slots (4 Charles Prime Rib, Carbone, Torrisi) the instant they drop, before other bots and humans can react.

Built to defeat [Imperva WAF](https://www.imperva.com/) bot detection through full Chrome browser impersonation at every layer: TLS fingerprint (JA3/JA4), HTTP/2 SETTINGS frame, header ordering, Client Hints, and Imperva session cookie management, all routed through benchmarked residential proxies.

## Quick Start

**1. Get your auth token** from [resy.com](https://resy.com). Log in, open DevTools (`F12`) → Console, paste:

```js
copy(apiAuthToken);console.log(apiAuthToken)
```

**2. Build and configure**

```bash
git clone git@github.com:gabehassan/table42.git && cd table42
go build -o table42 .
cp .env.example .env
# Edit .env: paste your auth token, set venue and date
```

**3. Book**

```bash
# Paste any Resy URL
./table42 snipe "https://resy.com/cities/new-york-ny/venues/lartusi-ny?date=2026-04-15&seats=2"

# Or use the interactive wizard
./table42 setup

# Or configure .env and run directly
./table42
```

The bot handles: Imperva cookies, Chrome fingerprinting, captcha bypass, and parallel booking.

## How It Works

Resy's API is protected by Imperva's Advanced Bot Protection. Standard HTTP clients are detected and blocked during drops. table42 impersonates Chrome 146 end-to-end:

| Detection Layer | What Imperva Checks | How table42 Bypasses It |
|---|---|---|
| **TLS** | JA3/JA4 fingerprint | Chrome 146 exact match via [tls-client](https://github.com/bogdanfinn/tls-client) |
| **HTTP/2** | SETTINGS frame, pseudo-header order | Chrome values (`WINDOW_SIZE=6MB`, `m,a,s,p` ordering) |
| **Headers** | Order, Client Hints, Sec-Fetch | Deterministic Chrome order, full `Sec-CH-UA` / `Sec-Fetch-*` |
| **Sessions** | Imperva cookies | Collected at warmup, persisted across requests |
| **IP** | Datacenter vs residential | Residential proxies with sticky sessions |
| **reCAPTCHA** | Venue-level captcha | Bypassed via undocumented `GET /3/details` endpoint |

### Booking Pipeline

```
POST /4/find ──→ GET /3/details ──→ POST /3/book
```

Monitors for slots, then fires the full pipeline instantly. No captcha delay. Multiple booking attempts through different proxy IPs in parallel, first success wins.

<details>
<summary>Proxies (optional, required on VPS)</summary>

Not needed on home networks. Required on VPS for competitive venues (Imperva blocks datacenter IPs).

Create `proxylist.txt` next to `.env`:

```bash
# Sticky sessions recommended (same IP for 30 min)
state.decodo.com:15001:user-USERNAME-sessionduration-30:PASSWORD
state.decodo.com:15002:user-USERNAME-sessionduration-30:PASSWORD
# ... add 6-10 endpoints

# Also supports URL format
http://user:pass@host:port
```

```bash
./table42 test-proxy   # Test and benchmark all proxies
```

At startup the bot benchmarks all proxies, assigns fastest to booking, next batch to monitoring, and drops failures. Falls back to direct if all proxies fail.

</details>

<details>
<summary>Drop-time mode</summary>

```bash
# .env
RESY_VENUE_ID=834
RESY_DATE=2026-04-22
RESY_DROP_TIME=2026-04-02T09:00:00-04:00
RESY_TIME=19:00
RESY_TIME_RANGE=17:00-22:00
RESY_PARTY_SIZE=2
RESY_TABLE_TYPE=Indoor Dining
```

```bash
./table42                       # Monitor until slots appear
./table42 --monitor-until 15m   # Stop 15 min after drop
./table42 --blind-fire          # Also fire at exact T-0
```

The bot sleeps until T-30s, warms all connections, then monitors target date ±1 day in parallel. When slots appear → book instantly → Discord webhook.

</details>

## Performance

| Metric | table42 | Python bots |
|--------|---------|-------------|
| Full pipeline (find → book) | **~1s** (proxy) / **~200ms** (direct) | ~2,500ms |
| JSON parse (99KB) | **32µs** | ~10ms |
| Parallel booking | **3 simultaneous IPs** | 1 sequential |

**Why:** zero-alloc hot path, byte-scan JSON parser (19x faster than `json.Unmarshal`), broadcast-pattern goroutines, GC suppression, spin-wait timing (33ns precision).

## Known Drop Times

| Restaurant | Drop Time (ET) | Days Out | Notes |
|---|---|---|---|
| 4 Charles Prime Rib | 9:00 AM | 20 | $5/pp reservation fee |
| Carbone | 10:00 AM | 30 | $50/pp deposit |
| Torrisi | 10:00 AM | 30 | Same group as Carbone |
| Tatiana | 12:00 PM | 27-28 | Also accepts phone reservations |

<details>
<summary>Commands</summary>

```bash
./table42                       # Run (mode from .env)
./table42 snipe <resy-url>      # Book from URL
./table42 setup                 # Interactive wizard
./table42 search <name>         # Search venues
./table42 test                  # Test Discord webhook
./table42 test-proxy            # Benchmark proxies
./table42 cancel                # List bookings
./table42 cancel <id>           # Cancel a booking
```

</details>

<details>
<summary>All environment variables</summary>

| Variable | Default | Description |
|----------|---------|-------------|
| `RESY_AUTH_TOKEN` | - | Auth token from resy.com DevTools |
| `RESY_EMAIL` / `RESY_PASSWORD` | - | Alternative to token |
| `RESY_VENUE_ID` | - | Numeric venue ID (required) |
| `RESY_DATE` | - | Target date `YYYY-MM-DD` (required) |
| `RESY_TIME` | - | Preferred time `HH:MM` |
| `RESY_TIME_RANGE` | - | Acceptable window, e.g. `17:00-22:00` |
| `RESY_PARTY_SIZE` | `2` | Number of guests |
| `RESY_TABLE_TYPE` | any | e.g. `Indoor Dining` |
| `RESY_DROP_TIME` | - | RFC3339 drop time |
| `RESY_SHOTS` | `3` | Parallel find requests |
| `RESY_MAX_BOOK` | `5` | Parallel booking attempts |
| `RESY_BLIND_FIRE` | `false` | Fire at exact T-0 |
| `RESY_MONITOR_UNTIL` | indefinite | e.g. `5m`, `1h` |
| `RESY_WEBHOOK` | - | Discord webhook URL |
| `RESY_PROXY_FILE` | `proxylist.txt` | Proxy list path |
| `RESY_PAYMENT_ID` | auto | `0` for no-deposit venues |
| `RESY_OUTPUT` | - | `json` for machine output |

</details>

## Acknowledgments

- [korbinschulz/resybot-open](https://github.com/korbinschulz/resybot-open) for discovering the `GET /3/details` reCAPTCHA bypass
- [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client) for Chrome TLS + HTTP/2 fingerprint impersonation

## Disclaimer

This software is for **personal use only**: booking reservations for yourself, on your own Resy account. Not for resale, not for commercial use. Reselling reservations may violate the [New York Restaurant Reservation Anti-Piracy Act](https://www.nysenate.gov/legislation/bills/2023/S9365) (fines up to $1,000/violation).

By using this software you accept all responsibility for compliance with Resy's Terms of Service, restaurant policies, and applicable law. The author provides no warranty and accepts no liability for account actions, financial charges, or missed reservations. **[Full legal terms →](LEGAL.md)**

## License

All rights reserved. You may view the source code but may not copy, modify, distribute, or use it without explicit written permission from the author.
