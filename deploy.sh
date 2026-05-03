#!/bin/sh
set -e

echo "==> Pulling latest code..."
git pull

echo "==> Building and restarting notify-bot..."
docker compose up -d --build

echo "==> Waiting for health..."
timeout=30
while [ $timeout -gt 0 ]; do
  if docker exec notify-bot wget -qO- http://localhost:9119/health 2>/dev/null | grep -q '"status":"ok"'; then
    echo "==> Deploy complete! Healthy."
    exit 0
  fi
  sleep 1
  timeout=$((timeout - 1))
done

echo "==> WARNING: Health check did not pass within 30s. Check: docker compose logs --tail 50 notify-bot"
exit 1
