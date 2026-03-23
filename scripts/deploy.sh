#!/usr/bin/env bash
# Deploy table42 to a VPS
# Usage: ./deploy.sh user@host

set -euo pipefail

HOST="${1:?Usage: ./deploy.sh user@host}"
REMOTE_DIR="~/table42"

echo "Building for Linux..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o table42-linux .

echo "Deploying to $HOST..."
ssh "$HOST" "mkdir -p $REMOTE_DIR"
scp table42-linux "$HOST:$REMOTE_DIR/table42"
scp .env "$HOST:$REMOTE_DIR/.env"
ssh "$HOST" "chmod +x $REMOTE_DIR/table42"

echo ""
echo "Deployed! To run:"
echo "  ssh $HOST"
echo "  cd table42"
echo "  ./table42"
echo ""
echo "To schedule for drop time:"
echo "  ssh $HOST"
echo "  cd table42 && nohup ./table42 > output.log 2>&1 &"
echo "  # or use: screen -S table42 -d -m ./table42"
