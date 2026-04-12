# MVVNotifier

Lightweight Telegram notification service for your VPS. Zero external dependencies — built on Go stdlib only.

Subscribers press `/start` on your bot and receive all notifications you send via HTTP or CLI.

## Quick Start (Docker)

```bash
docker run -d --name notifier \
  -e NOTIFY_BOT_TOKEN=your_token \
  -p 127.0.0.1:9119:9119 \
  -v notify-data:/data \
  --restart unless-stopped \
  ghcr.io/menkow/mvvnotifier:latest
```

## Quick Start (Binary)

```bash
git clone https://github.com/menkow/MVVNotifier.git
cd MVVNotifier
chmod +x install.sh
./install.sh
```

Then set your bot token and start the service:

```bash
sudo systemctl edit notify-bot
# Add: Environment=NOTIFY_BOT_TOKEN=your_token
sudo systemctl enable --now notify-bot
```

## Setup Telegram Bot

1. Open [@BotFather](https://t.me/BotFather), send `/newbot`
2. Choose a name and username
3. Copy the token

## Sending Notifications

### HTTP API

```bash
# simple message
curl localhost:9119/notify -d '{"text":"Backup complete"}'

# full options
curl localhost:9119/notify -d '{
  "text": "Disk usage at 95%",
  "level": "critical",
  "title": "Disk Alert",
  "service": "monitoring"
}'
```

### CLI

```bash
notify-bot send "Deploy finished" --level success --service deploy
notify-bot send "Error!" -l error -t "API down" -s backend
```

### From any language

```python
# Python
requests.post("http://127.0.0.1:9119/notify", json={
    "text": "Job done",
    "level": "info",
    "service": "etl"
})
```

```javascript
// Node.js
fetch("http://127.0.0.1:9119/notify", {
  method: "POST",
  body: JSON.stringify({ text: "Task complete", level: "success" }),
});
```

## API Reference

### `POST /notify`

| Field     | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `text`    | string | yes      | Notification message                             |
| `title`   | string | no       | Bold header                                      |
| `level`   | string | no       | `info`, `success`, `warn`, `error`, `critical`   |
| `service` | string | no       | Service name tag                                 |

Response:

```json
{ "sent": 3, "failed": 0, "total": 3 }
```

### `GET /health`

```json
{ "status": "ok", "subscribers": 3 }
```

## Bot Commands

| Command   | Description              |
|-----------|--------------------------|
| `/start`  | Subscribe to notifications |
| `/stop`   | Unsubscribe              |
| `/status` | Check subscription status |
| `/help`   | Show help                |

## Configuration

Environment variables (take priority):

| Variable            | Default | Description       |
|---------------------|---------|-------------------|
| `NOTIFY_BOT_TOKEN`  | —       | Telegram bot token |
| `NOTIFY_HTTP_PORT`  | `9119`  | HTTP listen port   |

Or use `config.json`:

```json
{
  "bot_token": "your_token",
  "http_port": 9119
}
```

## License

MIT
