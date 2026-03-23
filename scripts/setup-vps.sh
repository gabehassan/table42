#!/usr/bin/env bash
# Run this ON the VPS after first SSH login to set up the environment.
# Usage: ssh user@host 'bash -s' < setup-vps.sh

set -euo pipefail

echo "=== table42 VPS setup ==="

# Create directories
mkdir -p ~/table42
mkdir -p ~/.noresi

# System time sync (critical for spin-wait precision)
if command -v timedatectl &>/dev/null; then
    sudo timedatectl set-ntp true 2>/dev/null || true
    echo "NTP sync enabled"
fi

# Increase file descriptor limits (for parallel connections)
ulimit -n 65535 2>/dev/null || true

# Check Go (only needed if building on VPS)
if command -v go &>/dev/null; then
    echo "Go: $(go version)"
else
    echo "Go not installed (not needed — deploy pre-built binary)"
fi

# Network tuning for low-latency HTTP
if [ -w /proc/sys/net/ipv4/tcp_nodelay ] 2>/dev/null; then
    echo 1 | sudo tee /proc/sys/net/ipv4/tcp_nodelay >/dev/null
fi

# Test latency to api.resy.com
echo ""
echo "Testing latency to api.resy.com..."
if command -v curl &>/dev/null; then
    curl -sS -o /dev/null -w "DNS: %{time_namelookup}s | TCP: %{time_connect}s | TLS: %{time_appconnect}s | TTFB: %{time_starttransfer}s | Total: %{time_total}s\n" \
        -H 'Authorization: ResyAPI api_key="VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"' \
        -H 'Accept: application/json' \
        "https://api.resy.com/3/geoip"
fi

echo ""
echo "Setup complete. Deploy with: make deploy VPS_HOST=user@host"
echo "Or: ./deploy.sh user@host"
