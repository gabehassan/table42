# table42

Book restaurant reservations the instant they drop on Resy. A single Go binary that snipes competitive slots — like 4 Charles Prime Rib, Carbone, and Torrisi — in under 300ms, before other bots and humans can react.

> Looking for OpenTable? Check out [noresi](https://github.com/gabehassan/noresi) — same architecture, built for the OpenTable mobile API.

## Quick Start (Easy Mode)

**1. Get your auth token** — go to [resy.com](https://resy.com), log in, open DevTools (`F12`) → Console, paste this:

```js
fetch("https://api.resy.com/3/auth/refresh",{method:"POST",credentials:"include",headers:{Authorization:'ResyAPI api_key="VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"'}}).then(r=>r.json()).then(d=>{navigator.clipboard.writeText(d.token);console.log("Auth token copied to clipboard!")}).catch(()=>console.log("Not logged in — log in to resy.com first"))
```

**2. Build**

```bash
git clone git@github.com:gabehassan/table42.git && cd table42
go build -o table42 .
```

**3. Set your token**

```bash
cp .env.example .env
# Edit .env and paste your auth token as RESY_AUTH_TOKEN=...
```

**4. Book** — paste any Resy URL:

```bash
./table42 snipe "https://resy.com/cities/new-york-ny/venues/lartusi-ny?date=2026-04-15&seats=2"
```

Or use the interactive setup wizard:

```bash
./table42 setup
```

Or search for a restaurant:

```bash
./table42 search "carbone"
```

## Power User Setup

For competitive drops (4 Charles, Carbone, etc.) where milliseconds matter:

### Configure `.env`

```bash
# Auth (paste token from browser, or use email+password)
RESY_AUTH_TOKEN=eyJ0eXAi...

# Target
RESY_VENUE_ID=834                           # 4 Charles Prime Rib
RESY_DATE=2026-04-12
RESY_TIME=19:00                             # Preferred (slots sorted by proximity)
RESY_TIME_RANGE=17:00-22:00                 # Accept anything in this window
RESY_PARTY_SIZE=2
RESY_TABLE_TYPE=Indoor Dining

# Drop time — bot sleeps until this moment, then fires
RESY_DROP_TIME=2026-03-23T09:00:00-04:00

# Parallel attempts
RESY_SHOTS=3                                # 3 parallel find requests (optimal)
RESY_MAX_BOOK=5                             # Try 5 slots simultaneously

# CAPTCHA (optional — pre-solves before drop)
RESY_CAPSOLVER_KEY=CAP-...                  # capsolver.com, $0.001/solve

# Notifications
RESY_WEBHOOK=https://discord.com/api/webhooks/...

RESY_OUTPUT=json
```

### Run

```bash
./table42                    # Run (mode determined by config)
./table42 test               # Test webhooks
./table42 cancel             # List bookings
./table42 cancel all         # Cancel all bookings
```

### Deploy to VPS

For minimum latency, run on a VPS in `us-east-1` (Ashburn, VA — same datacenter as Resy's API):

```bash
make build-linux
./scripts/deploy.sh user@your-vps

# On the VPS:
ssh your-vps
cd ~/table42 && nohup ./table42 > output.log 2>&1 &
```

## How It Works

Three-stage pipeline: **Find** slots → get **Details** (book token) → **Book** the reservation.

```
  POST /4/find ──→ POST /3/details ──→ POST /3/book
     ~110ms            ~28ms              ~120ms

                   ~265ms total
```

### Drop-Time Timeline

```
T-60s  Pre-solve reCAPTCHA via CAPSolver (if configured)
T-30s  Warm HTTP/2 connection (TCP + TLS handshake)
       Pre-build booking payload, disable garbage collector
T-10s  Pre-poll every 150ms (catches early drops)
T-0s   Spin-wait fires with 33ns precision
       3 parallel find shots (broadcast pattern)
       5 parallel booking attempts on best slots
T+30s  Retry loop if no slots at T-0 (handles late drops)
```

### Three Modes

| Mode | Use | Trigger |
|------|-----|---------|
| **Sniper** | Known drop time | Set `RESY_DROP_TIME` |
| **Monitor** | Unknown drop / cancellations | Set `RESY_MONITOR=true` |
| **Immediate** | Slots available now | Neither set |

## Performance

Measured against the live Resy API from AWS us-east-1:

| Metric | table42 | Python bots |
|--------|---------|-------------|
| Find + Details | **138ms** | ~1,400ms |
| Full pipeline (find → book) | **265ms** | ~2,500ms |
| JSON parse (99KB) | 32us | ~10ms |
| Spin-wait precision | 33ns | ~1ms |

**~10x faster than the leading open-source alternatives.**

### Why

- DNS pinning — resolve once, skip DNS on every request
- HTTP/2 multiplexing — single TCP connection for all parallel shots
- Byte scanning — 19x faster than `json.Unmarshal`
- Pre-built payloads and headers — zero allocation at fire time
- GC suppression — no stop-the-world pauses during booking
- Broadcast firing — all goroutines unblock simultaneously via `close(ready)`
- Spin-wait — OS sleep until T-2ms, then busy-wait on a pinned thread

## Known Drop Times

| Restaurant | Drop Time (ET) | Days Out | Notes |
|---|---|---|---|
| 4 Charles Prime Rib | 9:00 AM | 20 | Verified/VIP priority system |
| Carbone | 10:00 AM | 30 | $50/pp deposit |
| Torrisi | 10:00 AM | 30 | Same group as Carbone |
| Tatiana | 12:00 PM | 27-28 | Also accepts phone reservations |

## Configuration Reference

<details>
<summary>All environment variables</summary>

### Authentication

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_EMAIL` | Yes* | — | Resy account email |
| `RESY_PASSWORD` | Yes* | — | Resy account password |
| `RESY_AUTH_TOKEN` | No | — | Skip login (valid ~45 days) |
| `RESY_PAYMENT_ID` | No | auto | `0` works for no-deposit venues |
| `RESY_ACCOUNT_NAME` | No | auto | Display name for webhooks |

*Not required if `RESY_AUTH_TOKEN` is set.

### Target

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_VENUE_ID` | Yes | — | Numeric venue ID |
| `RESY_DATE` | Yes | — | `YYYY-MM-DD` |
| `RESY_TIME` | No | — | Preferred `HH:MM` (sorted by proximity) |
| `RESY_TIME_RANGE` | No | — | Window, e.g. `17:00-22:00` |
| `RESY_PARTY_SIZE` | No | `2` | Guests |
| `RESY_TABLE_TYPE` | No | any | e.g. `Indoor Dining` |

### Sniper

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_DROP_TIME` | No | — | RFC3339: `2026-04-01T09:00:00-04:00` |
| `RESY_SHOTS` | No | `3` | Parallel find shots |
| `RESY_MAX_BOOK` | No | `5` | Parallel booking attempts |

### Monitor

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_MONITOR` | No | `false` | Enable polling mode |
| `RESY_MONITOR_INTERVAL` | No | `30` | Poll interval (seconds) |
| `RESY_PROXY_FILE` | No | — | Proxy list (one per line) |

### CAPTCHA

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_CAPSOLVER_KEY` | No | — | capsolver.com API key |
| `RESY_CAPTCHA_LEAD` | No | `60` | Seconds before drop to pre-solve |

### Output

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `RESY_WEBHOOK` | No | — | Discord or Slack webhook URL |
| `RESY_OUTPUT` | No | — | `json` for machine-readable stdout |
| `RESY_LOG_FILE` | No | `~/.noresi/table42.log` | Timing log path |

</details>

## Architecture

```
main.go        Core: config, HTTP client, find/details/book pipeline, timing
auth.go        Email+password login, token caching, payment method fetch
bookings.go    Booking persistence, cancel command
captcha.go     CAPSolver integration, pre-solve before drop
proxy.go       Proxy pool, monitor client, connection warming, headers
setup.go       Setup wizard, venue search, snipe-by-URL mode
bench_live.go  Live benchmark suite (build-ignored)
```

## Acknowledgments

This project was informed by research from several open-source Resy projects:

- [korbinschulz/resybot-open](https://github.com/korbinschulz/resybot-open) — API endpoint documentation, booking flow reference
- [Alkaar/resy-booking-bot](https://github.com/Alkaar/resy-booking-bot) — The most popular Resy bot (Scala), community discussions on rate limits and detection
- [daylamtayari/cierge](https://github.com/daylamtayari/cierge) — Most complete Go Resy API implementation, dynamic API key extraction
- [21Bruce/resolved-bot](https://github.com/21Bruce/resolved-bot) — Go-based Resy bot with venue search integration
- [bthuilot/book-it](https://github.com/bthuilot/book-it) — Go + PostgreSQL Resy booking system

## Disclaimer

This software is provided for **personal, educational use only**.

- **Personal use only.** Designed to help individuals book reservations for themselves. Not for commercial resale.
- **No resale.** Do not use this to acquire reservations for selling on any marketplace. Reselling may violate the New York Restaurant Reservation Anti-Piracy Act (2024), with fines up to $1,000 per violation.
- **Compliance.** You are responsible for complying with Resy's terms of service and each restaurant's reservation and cancellation policies.
- **Payment and charges.** Some restaurants require a deposit or credit card hold when booking. By using this software, you acknowledge that you may incur charges including deposits, cancellation fees, or no-show penalties as determined by the restaurant. The author is not responsible for any financial charges resulting from reservations made with this tool. Review the restaurant's policies before booking.
- **No warranty.** Provided "as is" without warranty. The author is not responsible for account suspensions, legal action, financial charges, or missed reservations.
- **Don't be a jerk.** If you book, show up. No-shows hurt restaurants and other diners. Cancel if plans change.

## License

All rights reserved. You may view the source code but may not copy, modify, distribute, or use it without explicit written permission from the author.
