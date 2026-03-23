# table42 — Resy reservation sniper

BINARY  = table42
LDFLAGS = -s -w

.PHONY: build run bench clean deploy

# Build optimized binary (stripped, no debug symbols)
build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

# Build for Linux (VPS deployment)
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-linux .

# Run locally (reads .env)
run: build
	./$(BINARY)

# Run benchmarks against live API
bench:
	go run bench_live.go

# Vet + build
check:
	go vet ./...
	go build -o $(BINARY) .

# Clean build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-linux

# Deploy to VPS (set VPS_HOST=user@host)
deploy: build-linux
	@if [ -z "$(VPS_HOST)" ]; then echo "Usage: make deploy VPS_HOST=user@host"; exit 1; fi
	scp $(BINARY)-linux $(VPS_HOST):~/table42/table42
	scp .env $(VPS_HOST):~/table42/.env
	ssh $(VPS_HOST) 'chmod +x ~/table42/table42'
	@echo "Deployed. SSH in and run: cd ~/table42 && ./table42"

# First-time VPS setup
setup:
	@if [ -z "$(VPS_HOST)" ]; then echo "Usage: make setup VPS_HOST=user@host"; exit 1; fi
	ssh $(VPS_HOST) 'bash -s' < scripts/setup-vps.sh
