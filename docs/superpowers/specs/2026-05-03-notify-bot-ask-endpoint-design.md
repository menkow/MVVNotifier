# Notify-bot `/ask` endpoint — design (v2)

Date: 2026-05-03
Service: MVVNotifier (this repo)
Target: VPS `83.219.248.126`, public domain `notify.siestalenses.ru`
Bot: `@mvv_notify_bot`

## Goal

Add a synchronous `/ask` endpoint to the existing notify-bot service. Claude sends a question via long-poll HTTP request; the question lands in the user's Telegram (text reply, sequential fallback, or inline buttons); the user's reply resolves the HTTP request, returning the answer back to Claude. Existing `/notify` and bot-subscription flow (`/start`, `/stop`, `/status`, `/help`) are untouched.

## Decisions (confirmed)

- **Sync model:** server-side long-poll. One HTTP request blocks until answer or timeout. No client-side polling.
- **HTTP methods:** both `POST` (JSON body) and `GET` (query string) accepted. The skill always uses POST — keeps text out of access logs and avoids URL-length issues with multiple buttons / cyrillic. GET stays as a fallback for ad-hoc shell smoke tests by the user.
- **Routing — hybrid:**
  - **Inline buttons** if the request includes `&button=` params: user taps a button, the `callback_data` becomes the answer.
  - **Reply-to** (always works): user taps "Reply" on the bot's question message and types an answer; bot maps `reply_to_message.message_id` → pending question.
  - **Sequential fallback:** if exactly one question is pending and the user types plain text (no reply, no command) — that text resolves the single pending question.
- **Auth:** none, same as `/notify`. Accepted risk: any caller knowing the URL can put a question into the user's Telegram.
- **Concurrency cap:** ≤ 5 simultaneous pending questions globally. 6th call returns `429`.
- **Default timeout:** 300s (5 min). Maximum: 600s (10 min) to keep within proxy limits.
- **Persistence:** pending questions are in-memory only. Container restart (e.g. auto-deploy mid-question) drops in-flight calls — Claude sees connection error and falls back to asking via chat. Acceptable.
- **Skill:** new separate skill `bot-mode`, **strictly modal**. The user explicitly does not want per-question triggers ("спроси меня через бот про X"). Instead, one phrase enables a session-wide mode where every clarifying question Claude would normally ask in chat is routed through `/ask`. Another phrase exits the mode. While the mode is on, Claude still uses chat for status/progress updates and final answers — only *questions to the user* go through `/ask`. The `notify-via-bot` skill stays in place separately for fire-and-forget alerts.

## API

The endpoint accepts both `POST` (primary, for the skill) and `GET` (fallback, for manual shell smoke tests). Same parameter set, same response.

### POST — primary

```
POST https://notify.siestalenses.ru/ask
Content-Type: application/json

{
  "text":    "<required>",
  "timeout": 300,
  "buttons": ["Yes", "No"],
  "level":   "info",
  "title":   "<optional>",
  "service": "<optional>"
}
```

### GET — fallback

`GET https://notify.siestalenses.ru/ask?text=...&timeout=...&button=Yes&button=No&level=...&title=...&service=...` — `button` is repeatable.

### Parameters (both methods)

| Param | Required | Default | Notes |
|---|---|---|---|
| `text` | yes | — | Question body. Same `Notification` formatting as `/notify` (escaped HTML, prefixed with level emoji + title). |
| `timeout` | no | `300` | Seconds. Clamped to `[5, 600]`. |
| `buttons` (POST) / `button` (GET) | no | — | List of strings. Each becomes a button; label = `callback_data` = the string itself. Up to 6 (single row). |
| `level` | no | `info` | Same set as `/notify`. |
| `title` | no | `UPPERCASE(level)` | Same as `/notify`. |
| `service` | no | — | Same as `/notify`. |

Successful response (HTTP 200):

```json
{"answer": "yes", "via": "button", "ms": 1843}
```

`via`: `"button"` | `"reply"` | `"text"`. `ms`: how long the user took to reply.

Error responses:

| Code | Body | Meaning |
|---|---|---|
| 400 | `{"error":"text is required"}` | Missing `text` |
| 400 | `{"error":"too many buttons","max":6}` | More than 6 `button` params |
| 408 | `{"error":"timeout","ms":300000}` | Timeout reached, no reply |
| 429 | `{"error":"rate limit exceeded","limit":"60/min"}` | Hit `/notify` global rate limit (shared bucket — see Risks) |
| 429 | `{"error":"too many concurrent questions","limit":5}` | 5 already pending |
| 503 | (connection drop) | Container restart mid-request |

Methods: `POST` (primary, JSON body) and `GET` (fallback, query string). Other methods → 405.

## Architecture

```text
                         +---------------------+
                         |  Claude Code (Bash) |
                         |  curl --max-time T  |
                         +----------+----------+
                                    |  POST /ask {text, buttons, ...}
                                    v
              Internet :443 (TCP, SNI)
                                    |
                          +---------+---------+
                          | HAProxy SNI split |
                          +---------+---------+
                                    |
                          +---------+---------+
                          |       Caddy       |
                          | reverse_proxy ->  |
                          |   notify-bot:9119 |
                          +---------+---------+
                                    |
                                    v
                       +-------------------------+
                       |       notify-bot        |
                       |                         |
                       |  /ask handler --------- |       Telegram Bot API
                       |    create pendingQ      |  <----------------------+
                       |    sendMessage w/       |                          |
                       |    reply_markup -------+----- sendMessage         |
                       |    block on chan       |                          |
                       |                         |                          |
                       |  pollBot goroutine     |  <----------------------+
                       |    getUpdates loop      |     callback_query OR    |
                       |    callback_query ----- |     message w/           |
                       |       resolve via       |     reply_to_message     |
                       |       callback_data     |                          |
                       |    reply-to message --- |                          |
                       |       resolve via       |                          |
                       |       byMsgID lookup    |                          |
                       |    plain text + 1       |                          |
                       |       pending --------- |                          |
                       |       sequential        |                          |
                       +-------------------------+
```

The existing `pollBot()` goroutine is the single consumer of Telegram updates. It dispatches `/start`/`/stop`/etc. as before, plus the three new branches for `/ask` resolution.

## Components

### 1. `main.go`

#### New types

```go
// Per-question state for /ask.
type pendingQ struct {
    id       string         // short random id (e.g. base32 of 8 random bytes), for diagnostics
    msgID    int64          // Telegram message_id of the sent question (for reply-to routing)
    answerCh chan askResult
    deadline time.Time
}

type askResult struct {
    answer string
    via    string // "button" | "reply" | "text"
}

// Global registry. Two indices: by id (for cleanup), by msgID (for reply-to lookup).
var asks = struct {
    sync.Mutex
    byID    map[string]*pendingQ
    byMsgID map[int64]*pendingQ
}{
    byID:    make(map[string]*pendingQ),
    byMsgID: make(map[int64]*pendingQ),
}
```

Concurrency cap is enforced by checking `len(asks.byID) >= 5` under the mutex.

#### Update struct extension

The existing `Update` struct needs `CallbackQuery *callbackQuery` added. New field on `Message`: `ReplyToMessage *struct{ MessageID int64 }`. Match the JSON field names exactly (`reply_to_message`, `message_id`).

```go
type CallbackQuery struct {
    ID      string `json:"id"`
    Data    string `json:"data"`
    Message *struct {
        MessageID int64 `json:"message_id"`
        Chat      struct {
            ID int64 `json:"id"`
        } `json:"chat"`
    } `json:"message"`
}

type Update struct {
    UpdateID      int64          `json:"update_id"`
    Message       *Message       `json:"message"`
    CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
    MessageID      int64 `json:"message_id"`
    Chat           struct {
        ID        int64  `json:"id"`
        FirstName string `json:"first_name"`
        Username  string `json:"username"`
    } `json:"chat"`
    Text           string `json:"text"`
    ReplyToMessage *struct {
        MessageID int64 `json:"message_id"`
    } `json:"reply_to_message"`
}
```

(Existing inline anonymous Message struct gets refactored into a named type for readability.)

#### New TG methods

```go
func (t *TG) SendWithKeyboard(chatID int64, text string, buttons []string) (int64, error)
// Returns the message_id of the sent message. If buttons is empty, sends plain HTML message
// (still returns message_id so /ask can populate byMsgID for reply-to routing).

func (t *TG) AnswerCallbackQuery(callbackID string) error
// Acknowledges the button press so Telegram stops the spinner.

func (t *TG) EditMessageRemoveKeyboard(chatID, messageID int64, finalText string) error
// Edits the original question to remove inline_keyboard and append "\n\n✅ Answered: <choice>".
```

#### New handler

`handleAsk(w, r, tg, store, askLimiter)` — wired in `main()` next to `handleNotify`. Reuses the same `notifyLimiter` token bucket (no separate `/ask` rate limit; the per-call cost is the concurrency cap, not request rate).

Flow:
1. Reject methods other than POST and GET → 405 with `Allow: GET, POST`.
2. Parse parameters:
   - **POST:** `Content-Type` must be `application/json`; decode body into a struct with `text`, `timeout`, `buttons` (`[]string`), `level`, `title`, `service`.
   - **GET:** read same fields from `r.URL.Query()`; `buttons` comes from `r.URL.Query()["button"]` (repeatable). Convert `timeout` to int.
   - Single helper `parseAskRequest(r) (askReq, error)` keeps both paths converging on the same struct.
3. Validate: `text != ""` → else 400; `len(buttons) <= 6` → else 400; clamp `timeout` to `[5, 600]`.
4. `notifyLimiter.allow()` → 429 if not.
5. Concurrency check: `len(asks.byID) >= 5` → 429.
6. Format question body via `Notification.Format()`. If no buttons, append `<i>(Reply to this message or type your answer)</i>`.
7. Generate `id`. For each subscriber in `store.List()`:
   - `tg.SendWithKeyboard(chatID, body, buttons)` → returns `msgID`.
   - On the first subscriber (or current single subscriber), register `pendingQ{id, msgID, answerCh, deadline}` in `asks.byID` and `asks.byMsgID`.
   - Note: with multiple subscribers we'd need per-subscriber state. **For v2 we assume one subscriber** (current reality) — register only once, with the first sent message_id. The subscriber count check on entry guards against the multi-subscriber path firing accidentally.
8. Wait on `answerCh` with `select` against a `time.NewTimer(timeoutDur)`. On timer fire — clean up, return 408.
9. On answer — clean up registry, return 200 with `{answer, via, ms}`.

Cleanup is always via a `defer` that removes from both maps under the mutex.

#### `pollBot()` extension

Add at top of the per-update loop:

```go
if u.CallbackQuery != nil {
    asks.Lock()
    msgID := u.CallbackQuery.Message.MessageID
    p, ok := asks.byMsgID[msgID]
    asks.Unlock()
    if ok {
        // Resolve, then ack + edit
        select {
        case p.answerCh <- askResult{answer: u.CallbackQuery.Data, via: "button"}:
        default:
        }
        tg.AnswerCallbackQuery(u.CallbackQuery.ID)
        tg.EditMessageRemoveKeyboard(u.CallbackQuery.Message.Chat.ID, msgID,
            fmt.Sprintf("✅ Answered: %s", escapeHTML(u.CallbackQuery.Data)))
    } else {
        // stale button press, ack quietly
        tg.AnswerCallbackQuery(u.CallbackQuery.ID)
    }
    continue
}
```

After the `if u.Message == nil { continue }` line, add (before the slash-command switch):

```go
// reply-to → resolve specific pending
if u.Message.ReplyToMessage != nil {
    asks.Lock()
    p, ok := asks.byMsgID[u.Message.ReplyToMessage.MessageID]
    asks.Unlock()
    if ok {
        select {
        case p.answerCh <- askResult{answer: u.Message.Text, via: "reply"}:
        default:
        }
        tg.EditMessageRemoveKeyboard(u.Message.Chat.ID, u.Message.ReplyToMessage.MessageID,
            fmt.Sprintf("✅ Answered: %s", escapeHTML(truncate(u.Message.Text, 80))))
        continue
    }
    // no matching pending — fall through to normal command handling
}

// sequential fallback: not a slash command + exactly one pending → resolve it
if !strings.HasPrefix(cmd, "/") {
    asks.Lock()
    var only *pendingQ
    if len(asks.byID) == 1 {
        for _, p := range asks.byID {
            only = p
        }
    }
    asks.Unlock()
    if only != nil {
        select {
        case only.answerCh <- askResult{answer: cmd, via: "text"}:
        default:
        }
        tg.EditMessageRemoveKeyboard(u.Message.Chat.ID, only.msgID,
            fmt.Sprintf("✅ Answered: %s", escapeHTML(truncate(cmd, 80))))
        continue
    }
}
```

`truncate(s, n)` is a small helper that returns `s` or `s[:n]+"..."`.

#### Routing

`mux.HandleFunc("/ask", ...)` next to existing `/notify` and `/health`.

### 2. HAProxy

`/root/haproxy/haproxy.cfg` — verify and bump if needed:

```haproxy
defaults
    timeout client  11m
    timeout server  11m
    timeout connect 5s
```

Current values must be ≥ 11m to allow a 10-min long-poll plus margin. If shorter, raise. Frontend `https_in` works in TCP mode and inherits these defaults.

Backup `haproxy.cfg.bak.<ts>` before edit. Restart `docker restart haproxy` (haproxy doesn't auto-reload).

### 3. Caddy

`/root/caddy/Caddyfile`:

```caddyfile
notify.siestalenses.ru {
    reverse_proxy notify-bot:9119 {
        transport http {
            read_timeout 11m
            write_timeout 11m
        }
    }
}
```

The default Caddy reverse_proxy has no timeouts on read/write but the `transport http` defaults are short for connect/idle. Explicit values guarantee long-poll works regardless of upstream timing. Validate with `docker exec caddy caddy validate --config /etc/caddy/Caddyfile`, then `caddy reload`.

### 4. New skill — `~/.claude/skills/bot-mode/SKILL.md` (modal)

User-level skill, distinct from `notify-via-bot`, **strictly modal**.

**Triggers (mode-on):** "перейди в режим бота", "включи бот-мод", "общайся через бот", "режим телеги", "управляй через бот", "turn on bot mode", "switch to telegram mode".

**Triggers (mode-off):** "выключи режим бота", "обратно в чат", "stop bot mode", "exit bot mode".

**Behavior while mode is on:** Every clarifying question Claude would normally ask the user in chat is instead routed through `POST /ask`. Status updates, progress reports, and final answers stay in chat. Notifications about completed long stages still go via `notify-via-bot` (separate concern). When the user replies in Telegram, Claude continues with the answer in the next chat turn.

The mode is **session-scoped**, kept in the conversation context. There's no persistent flag — when the session ends, the mode is gone. If the user wants it on by default for every session, they would need a hook in `~/.claude/settings.json` (out of scope for this spec).

**Skill body covers:**
- `POST` is the only way to call. GET is documented in skill only as a fallback for manual shell smoke tests by the user.
- `--max-time` ≥ body `timeout` + 10 seconds.
- **UTF-8 caveat (critical):** bash on Windows mangles non-ASCII inside `-d '...'`. For any non-ASCII text (Russian, emoji, accents, CJK), the skill must build the JSON via the **Write tool** to a temp file, then `curl --data-binary @file`. Same constraint as already documented in `notify-via-bot/SKILL.md`.
- Default `timeout=300`, range `[5, 600]`.
- When to use `buttons` (yes/no/N choices) vs free-form (open-ended).
- Handling 408 (no reply): exit mode for this question, ask in chat, optionally re-enable mode if the user wants.
- Handling 429 (concurrency cap): wait briefly, retry once, then fall back to chat.
- Handling 5xx / connection drop: assume container restarted by auto-deploy; fall back to chat.
- Question phrasing in mode: ONE question per call, concrete, echo consequences if action is irreversible.

### 5. Tests

The existing `ratelimit_test.go` style — no full integration tests for /ask (would require Telegram or a fake client). Cover what's testable in isolation:

- `TestParseAskRequest_POST` — JSON body decodes into the same `askReq` as GET; bad JSON → 400.
- `TestParseAskRequest_GET` — `button` repeated → `[]string`; same fields populated.
- `TestAskTimeoutClamp` — `timeout=0`, `timeout=99999` → clamped to `[5, 600]` (both methods).
- `TestAskTooManyButtons` — 7 buttons → 400.
- `TestAskConcurrencyCap` — fill 5, 6th rejected fast (no Telegram call attempted).
- `TestSequentialFallback` — single pending + plain text input resolves it.
- `TestReplyRouting` — two pending Q's, reply to second message_id resolves correctly.

The handler itself is testable by injecting a fake `*TG` (interface extraction or a small `tgSender` interface for the methods used). Skip integration testing against real Telegram — done manually via the deployment smoke test below.

## Deployment sequence

Each step is reversible. Halt and roll back if any step fails.

0. **Local repo:** apply main.go changes, write the new tests, `go test ./...`, `go build`. Commit on a feature branch.
1. **PR + merge to main.** Auto-deploy cron pulls within a minute and runs `./deploy.sh`. Watch `/var/log/notify-bot_deploy.log` for "Deploy complete! Healthy.".
2. **HAProxy:** ssh in, backup `haproxy.cfg`, ensure `timeout client/server >= 11m`, `docker restart haproxy`. Regression: `curl -I https://siestalenses.ru` and `curl -I https://cloud.siestalenses.ru` still 200/3xx.
3. **Caddy:** backup `Caddyfile`, add the `transport http` block to the `notify.siestalenses.ru` site, validate, reload. Regression: `curl -I https://notify.siestalenses.ru/health` → 200.
4. **Smoke test (manual, from local):**
   - **POST free-form:** `curl -s -X POST --max-time 310 'https://notify.siestalenses.ru/ask' -H 'Content-Type: application/json' -d '{"text":"Smoke test free-form","timeout":300}'` — reply something in Telegram, check JSON returned with `via:"reply"` or `via:"text"`.
   - **POST buttons:** add `"buttons":["Yes","No"]` to the JSON body — tap a button, check `via:"button"` and the `answer` matches.
   - **GET fallback (verify it still works):** `curl -sG --max-time 310 'https://notify.siestalenses.ru/ask' --data-urlencode 'text=GET fallback' --data-urlencode 'timeout=60'`.
   - **408 path:** send a question, don't reply, wait ~5 min, expect 408 + `{"error":"timeout"}`.
   - **Concurrency:** kick off 6 in parallel, expect one to return 429.
5. **Skill:** write `~/.claude/skills/bot-mode/SKILL.md`. New Claude Code sessions pick it up automatically.

## Rollback

| Failure point | Rollback |
|---|---|
| Step 1 (code/deploy) | `git revert <merge>` on main. Cron picks up within a minute and rebuilds. |
| Step 2 (HAProxy) | `cp haproxy.cfg.bak.<ts> haproxy.cfg && docker restart haproxy` |
| Step 3 (Caddy) | `cp Caddyfile.bak.<ts> Caddyfile && docker exec caddy caddy reload --config /etc/caddy/Caddyfile` |
| Step 4 (smoke fail) | If specifically `/ask` is broken, revert at Step 1. `/notify` is unaffected. |

`/ask` is purely additive. Existing `/notify`, bot subscriptions, and rate-limit are untouched.

## Accepted risks

- **Open `/ask`.** Anyone with the URL can spawn questions (capped at 5 concurrent, 60/min shared bucket). Mitigation: same as `/notify` — public URL, no auth, accepted by user.
- **In-memory only.** Container restart = lost in-flight asks. Acceptable: auto-deploy is rare, Claude falls back to chat on connection drop.
- **Sequential fallback ambiguity.** If multiple Claude tasks send `/ask` near-simultaneously, sequential fallback won't apply (must use reply or button). Documented in skill.
- **Telegram deletion of message.** If user deletes the bot's question message, reply-to and button paths break for that question; only sequential fallback (when applicable) and timeout remain. Acceptable.
- **Shared rate-limit bucket with `/notify`.** A flood on either endpoint can starve the other for up to a minute. Acceptable; could split buckets later if needed.
- **Single-subscriber assumption.** With more than one subscriber, only the first gets the question and only their reply counts. Documented in code; revisit when subscriber count > 1.
