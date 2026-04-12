#!/bin/bash
set -e

echo "=== notify-bot installer ==="

# check go
if ! command -v go &>/dev/null; then
    echo "Go not found. Installing..."
    wget -q https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -O /tmp/go.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    echo "Go installed: $(go version)"
fi

# build
echo "Building..."
go build -o notify-bot .
echo "Built: ./notify-bot"

# install
sudo mkdir -p /opt/notify-bot
sudo cp notify-bot /opt/notify-bot/
sudo cp notify-bot.service /etc/systemd/system/

# create user if needed
if ! id notify-bot &>/dev/null; then
    sudo useradd -r -s /usr/sbin/nologin notify-bot
fi
sudo chown -R notify-bot:notify-bot /opt/notify-bot

# symlink for CLI usage
sudo ln -sf /opt/notify-bot/notify-bot /usr/local/bin/notify-bot

echo ""
echo "=== Almost done! ==="
echo ""
echo "1. Set your bot token:"
echo "   sudo systemctl edit notify-bot"
echo "   Add: Environment=NOTIFY_BOT_TOKEN=your_token_here"
echo ""
echo "2. Start the service:"
echo "   sudo systemctl daemon-reload"
echo "   sudo systemctl enable --now notify-bot"
echo ""
echo "3. Check status:"
echo "   sudo systemctl status notify-bot"
echo ""
echo "4. Test:"
echo '   curl -s localhost:9119/notify -d '\''{"text":"Hello!","level":"info"}'\'''
echo '   notify-bot send "Hello from CLI!" --level info'
