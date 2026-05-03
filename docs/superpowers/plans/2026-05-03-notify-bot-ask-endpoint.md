# Notify-bot /ask Endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a long-poll `/ask` endpoint to notify-bot that lets Claude ask the user a question and receive their Telegram answer synchronously over a single HTTP request.

**Architecture:** Server-side long-poll. The handler sends a question to all subscribers, registers a `pendingQ` in two indices (`byID`, `byMsgID`), and blocks on a result channel until one of three resolution paths fires: inline-button click (`callback_query.data`), reply to the question message (`reply_to_message.message_id`), or sequential text fallback (one pending + plain text). Default timeout 300s, max 600s. In-memory only — container restart drops in-flight requests.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`, `sync`, `time`, `crypto/rand`, `encoding/base32`), Telegram Bot API methods (`sendMessage`, `getUpdates`, `answerCallbackQuery`, `editMessageText`, `editMessageReplyMarkup`), no new dependencies.

**Spec:** [`docs/superpowers/specs/2026-05-03-notify-bot-ask-endpoint-design.md`](../specs/2026-05-03-notify-bot-ask-endpoint-design.md)

---

## File Structure

Files modified in repo (`MVVNotifier`):
- `main.go` — refactor `Update`/`Message` to named types, add `CallbackQuery`, add 3 TG methods, add `pendingQ` registry + `tgSender` interface, add `parseAskRequest`, add `handleAsk`, wire `/ask` route, extend `pollBot()` for callback / reply-to / sequential fallback.

Files created in repo:
- `ask_test.go` — unit tests for parsing, clamping, registry, routing logic.

Files created outside repo:
- `~/.claude/skills/bot-mode/SKILL.md` — user-level skill.

Files modified on server (not in repo):
- `/root/haproxy/haproxy.cfg` — bump `timeout client/server` to ≥ 11m.
- `/root/caddy/Caddyfile` — add `transport http { read_timeout 11m, write_timeout 11m }` block to the `notify.siestalenses.ru` site.

---

## Task 1: Refactor Update/Message into named types, add CallbackQuery + ReplyToMessage fields

**Files:**
- Modify: `main.go` (around lines 184–207, the `Update` struct and `GetUpdates`)

This is a non-behavioral refactor. The current `Update` has an inline anonymous `Message` struct. We pull it out into a named type and add the new fields the rest of the plan needs.

- [ ] **Step 1: Replace the `Update` block with named types**

Replace:

```go
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}
```

with:

```go
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID      int64       `json:"message_id"`
	Chat           Chat        `json:"chat"`
	Text           string      `json:"text"`
	ReplyToMessage *ReplyMeta  `json:"reply_to_message"`
}

type Chat struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type ReplyMeta struct {
	MessageID int64 `json:"message_id"`
}

type CallbackQuery struct {
	ID      string           `json:"id"`
	Data    string           `json:"data"`
	Message *CallbackMessage `json:"message"`
}

type CallbackMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
}
```

- [ ] **Step 2: Update `pollBot` references to the inline struct**

Inside `pollBot`, the existing code does `u.Message.Chat.ID` and `u.Message.Chat.Username`. Those still work because `Message.Chat` is a named `Chat` struct with the same fields. No code changes needed in `pollBot` for this step — only the types changed. Verify with build.

- [ ] **Step 3: Build and run existing tests**

Run: `go build ./...` — must succeed.
Run: `go test ./...` — existing `ratelimit_test.go` tests must still pass.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "refactor: extract named Message/Update types, prep for /ask"
```

---

## Task 2: Add Telegram API methods (SendWithKeyboard, AnswerCallbackQuery, EditMessageRemoveKeyboard)

**Files:**
- Modify: `main.go` (around the existing `(t *TG) Send` method, ~line 175)

These are pure-additive methods on `*TG`. No tests in this task — they make HTTP calls to Telegram and will be exercised in the smoke test.

- [ ] **Step 1: Add `SendWithKeyboard`**

After the existing `Send` method, add:

```go
// SendWithKeyboard sends an HTML-formatted message with optional inline buttons.
// Each button's label is also its callback_data — keeps things simple.
// Returns the message_id of the sent message (used for reply-to routing).
func (t *TG) SendWithKeyboard(chatID int64, text string, buttons []string) (int64, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if len(buttons) > 0 {
		row := make([]map[string]string, 0, len(buttons))
		for _, b := range buttons {
			row = append(row, map[string]string{"text": b, "callback_data": b})
		}
		payload["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]string{row},
		}
	}
	raw, err := t.api("sendMessage", payload)
	if err != nil {
		return 0, err
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}
```

- [ ] **Step 2: Add `AnswerCallbackQuery`**

```go
// AnswerCallbackQuery acknowledges a button press so Telegram stops the loading
// spinner. Telegram requires this within 30 seconds.
func (t *TG) AnswerCallbackQuery(callbackID string) error {
	_, err := t.api("answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
	})
	return err
}
```

- [ ] **Step 3: Add `EditMessageRemoveKeyboard`**

```go
// EditMessageRemoveKeyboard rewrites the question message: replaces text with
// finalText and removes any inline keyboard, so the user can't answer twice.
func (t *TG) EditMessageRemoveKeyboard(chatID, messageID int64, finalText string) error {
	_, err := t.api("editMessageText", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"text":         finalText,
		"parse_mode":   "HTML",
		"reply_markup": map[string]any{"inline_keyboard": [][]any{}},
	})
	return err
}
```

- [ ] **Step 4: Build**

Run: `go build ./...` — must succeed.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat(tg): SendWithKeyboard, AnswerCallbackQuery, EditMessageRemoveKeyboard"
```

---

## Task 3: Add pendingQ registry types and helper

**Files:**
- Modify: `main.go` (new section, place above `// --- Main ---`)
- Create: `ask_test.go`

This adds the in-memory pending-question registry — the data structure both `handleAsk` and `pollBot` will use.

- [ ] **Step 1: Write the failing test**

Create `ask_test.go`:

```go
package main

import (
	"testing"
	"time"
)

func TestPendingQRegistry_AddAndLookup(t *testing.T) {
	reg := newPendingRegistry()

	p := &pendingQ{
		id:       "abc12345",
		msgID:    12345,
		answerCh: make(chan askResult, 1),
		deadline: time.Now().Add(5 * time.Minute),
	}
	reg.add(p)

	if got := reg.byID("abc12345"); got != p {
		t.Fatalf("byID lookup mismatch")
	}
	if got := reg.byMsg(12345); got != p {
		t.Fatalf("byMsg lookup mismatch")
	}
	if reg.count() != 1 {
		t.Fatalf("count = %d, want 1", reg.count())
	}
}

func TestPendingQRegistry_Remove(t *testing.T) {
	reg := newPendingRegistry()
	p := &pendingQ{id: "x", msgID: 1, answerCh: make(chan askResult, 1)}
	reg.add(p)
	reg.remove(p)

	if reg.byID("x") != nil || reg.byMsg(1) != nil {
		t.Fatalf("entry not removed")
	}
	if reg.count() != 0 {
		t.Fatalf("count = %d, want 0", reg.count())
	}
}

func TestPendingQRegistry_OnlyOne(t *testing.T) {
	reg := newPendingRegistry()
	if reg.onlyOne() != nil {
		t.Fatalf("empty registry should return nil")
	}
	p := &pendingQ{id: "x", msgID: 1, answerCh: make(chan askResult, 1)}
	reg.add(p)
	if reg.onlyOne() != p {
		t.Fatalf("single entry should be returned")
	}
	reg.add(&pendingQ{id: "y", msgID: 2, answerCh: make(chan askResult, 1)})
	if reg.onlyOne() != nil {
		t.Fatalf("two entries should return nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./...`
Expected: FAIL — `pendingQ`, `askResult`, `newPendingRegistry` undefined.

- [ ] **Step 3: Add the registry to `main.go`**

Place above `// --- Main ---`:

```go
// --- Ask Registry ---

type askResult struct {
	answer string
	via    string // "button" | "reply" | "text"
}

type pendingQ struct {
	id       string
	msgID    int64
	answerCh chan askResult
	deadline time.Time
}

type pendingRegistry struct {
	mu      sync.Mutex
	id2q    map[string]*pendingQ
	msg2q   map[int64]*pendingQ
}

func newPendingRegistry() *pendingRegistry {
	return &pendingRegistry{
		id2q:  make(map[string]*pendingQ),
		msg2q: make(map[int64]*pendingQ),
	}
}

func (r *pendingRegistry) add(p *pendingQ) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.id2q[p.id] = p
	r.msg2q[p.msgID] = p
}

func (r *pendingRegistry) remove(p *pendingQ) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.id2q, p.id)
	delete(r.msg2q, p.msgID)
}

func (r *pendingRegistry) byID(id string) *pendingQ {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id2q[id]
}

func (r *pendingRegistry) byMsg(msgID int64) *pendingQ {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.msg2q[msgID]
}

func (r *pendingRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.id2q)
}

// onlyOne returns the sole pending question if there is exactly one,
// otherwise nil. Used by the sequential-fallback path in pollBot.
func (r *pendingRegistry) onlyOne() *pendingQ {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.id2q) != 1 {
		return nil
	}
	for _, p := range r.id2q {
		return p
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS — all 3 new tests + existing rate-limit tests.

- [ ] **Step 5: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat: pendingQ registry for /ask"
```

---

## Task 4: Implement parseAskRequest (POST + GET)

**Files:**
- Modify: `main.go`
- Modify: `ask_test.go`

A single helper unmarshals POST JSON or pulls GET query params into the same `askReq` struct. This is the input-side of `handleAsk`.

- [ ] **Step 1: Write failing tests**

Append to `ask_test.go`:

```go
import (
	"net/http"
	"net/http/httptest"
	"strings"
)

func TestParseAskRequest_POST(t *testing.T) {
	body := `{"text":"hi","timeout":120,"buttons":["Yes","No"],"level":"info","title":"Q","service":"s"}`
	r := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	got, err := parseAskRequest(r)
	if err != nil {
		t.Fatalf("parseAskRequest: %v", err)
	}
	if got.Text != "hi" || got.Timeout != 120 || got.Level != "info" || got.Title != "Q" || got.Service != "s" {
		t.Fatalf("scalar fields: %+v", got)
	}
	if len(got.Buttons) != 2 || got.Buttons[0] != "Yes" || got.Buttons[1] != "No" {
		t.Fatalf("buttons: %+v", got.Buttons)
	}
}

func TestParseAskRequest_POST_BadJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader("{not json"))
	r.Header.Set("Content-Type", "application/json")
	if _, err := parseAskRequest(r); err == nil {
		t.Fatalf("expected error for bad JSON")
	}
}

func TestParseAskRequest_GET(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ask?text=hi&timeout=60&button=Yes&button=No&level=warn&title=Q&service=s", nil)
	got, err := parseAskRequest(r)
	if err != nil {
		t.Fatalf("parseAskRequest: %v", err)
	}
	if got.Text != "hi" || got.Timeout != 60 || got.Level != "warn" || got.Title != "Q" || got.Service != "s" {
		t.Fatalf("scalar fields: %+v", got)
	}
	if len(got.Buttons) != 2 || got.Buttons[0] != "Yes" || got.Buttons[1] != "No" {
		t.Fatalf("buttons: %+v", got.Buttons)
	}
}

func TestParseAskRequest_GET_NoTimeout(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ask?text=hi", nil)
	got, err := parseAskRequest(r)
	if err != nil {
		t.Fatalf("parseAskRequest: %v", err)
	}
	if got.Timeout != 0 { // 0 means "use default" — clamp happens later
		t.Fatalf("Timeout = %d, want 0", got.Timeout)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run ParseAskRequest`
Expected: FAIL — `parseAskRequest`, `askReq` undefined.

- [ ] **Step 3: Add `askReq` and `parseAskRequest`**

In `main.go`, place above the registry:

```go
type askReq struct {
	Text    string   `json:"text"`
	Timeout int      `json:"timeout"`
	Buttons []string `json:"buttons"`
	Level   string   `json:"level"`
	Title   string   `json:"title"`
	Service string   `json:"service"`
}

func parseAskRequest(r *http.Request) (askReq, error) {
	var req askReq
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, err
		}
		return req, nil
	}
	// GET: pull from query string
	q := r.URL.Query()
	req.Text = q.Get("text")
	req.Level = q.Get("level")
	req.Title = q.Get("title")
	req.Service = q.Get("service")
	req.Buttons = q["button"]
	if t := q.Get("timeout"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			req.Timeout = v
		}
	}
	return req, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run ParseAskRequest`
Expected: PASS — 4 new tests.

- [ ] **Step 5: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat: parseAskRequest (POST JSON + GET query)"
```

---

## Task 5: Implement handleAsk via tgSender interface

**Files:**
- Modify: `main.go`
- Modify: `ask_test.go`

`handleAsk` blocks on the channel. To test cleanly without real Telegram, extract a `tgSender` interface for the methods used; tests provide a fake that records calls and lets the test resolve the channel manually.

- [ ] **Step 1: Add the interface in `main.go`**

```go
type tgSender interface {
	SendWithKeyboard(chatID int64, text string, buttons []string) (int64, error)
	AnswerCallbackQuery(callbackID string) error
	EditMessageRemoveKeyboard(chatID, messageID int64, finalText string) error
}
```

`*TG` already satisfies this. Verify with build.

- [ ] **Step 2: Add helpers in `main.go`**

```go
func newAskID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func clampTimeout(t int) time.Duration {
	if t < 5 {
		t = 300 // default
	}
	if t > 600 {
		t = 600
	}
	return time.Duration(t) * time.Second
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
```

Add imports `"crypto/rand"` and `"encoding/base32"` to the import block.

Note: `clampTimeout` treats `< 5` as "missing/invalid" and uses 300. Validation for `Buttons` length is in `handleAsk`.

- [ ] **Step 3: Write failing tests for `handleAsk` (validation paths)**

Append to `ask_test.go`:

```go
import (
	"context"
	"encoding/json"
	"sync"
)

// fakeTG records calls and lets the test resolve pending questions externally.
type fakeTG struct {
	mu              sync.Mutex
	sendCalls       int
	nextMsgID       int64
	answerAcks      []string
	editFinals      map[int64]string
}

func newFakeTG() *fakeTG {
	return &fakeTG{nextMsgID: 1000, editFinals: make(map[int64]string)}
}

func (f *fakeTG) SendWithKeyboard(chatID int64, text string, buttons []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls++
	f.nextMsgID++
	return f.nextMsgID, nil
}

func (f *fakeTG) AnswerCallbackQuery(callbackID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answerAcks = append(f.answerAcks, callbackID)
	return nil
}

func (f *fakeTG) EditMessageRemoveKeyboard(chatID, messageID int64, finalText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editFinals[messageID] = finalText
	return nil
}

func TestHandleAsk_MethodNotAllowed(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	r := httptest.NewRequest(http.MethodPut, "/ask", nil)
	w := httptest.NewRecorder()
	handleAsk(w, r, tg, store, limiter, reg)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow header = %q", got)
	}
}

func TestHandleAsk_TextRequired(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	r := httptest.NewRequest(http.MethodPost, "/ask",
		strings.NewReader(`{"timeout":5}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAsk(w, r, tg, store, limiter, reg)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleAsk_TooManyButtons(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	body := `{"text":"hi","timeout":5,"buttons":["1","2","3","4","5","6","7"]}`
	r := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAsk(w, r, tg, store, limiter, reg)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleAsk_ConcurrencyCap(t *testing.T) {
	reg := newPendingRegistry()
	// Fill registry with 5 fake pending entries
	for i := 0; i < 5; i++ {
		reg.add(&pendingQ{
			id:       newAskID(),
			msgID:    int64(1000 + i),
			answerCh: make(chan askResult, 1),
		})
	}
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	r := httptest.NewRequest(http.MethodPost, "/ask",
		strings.NewReader(`{"text":"hi","timeout":5}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAsk(w, r, tg, store, limiter, reg)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if tg.sendCalls != 0 {
		t.Fatalf("sendCalls = %d, want 0 (should reject before sending)", tg.sendCalls)
	}
}

func TestHandleAsk_TimeoutPath(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	r := httptest.NewRequest(http.MethodPost, "/ask",
		strings.NewReader(`{"text":"hi","timeout":5}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	start := time.Now()
	handleAsk(w, r, tg, store, limiter, reg)
	elapsed := time.Since(start)

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", w.Code)
	}
	if elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("elapsed = %s, want ~5s", elapsed)
	}
	// registry must be cleaned up
	if reg.count() != 0 {
		t.Fatalf("registry not cleaned, count = %d", reg.count())
	}
}

func TestHandleAsk_AnswerPath(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	store := NewStore(t.TempDir() + "/subs.json")
	store.Add(42)
	limiter := newBucket(60, time.Second)

	r := httptest.NewRequest(http.MethodPost, "/ask",
		strings.NewReader(`{"text":"hi","timeout":30,"buttons":["Yes","No"]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Resolve the pending question after a short delay
	go func() {
		// Wait for the handler to register the pending entry
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var p *pendingQ
		for ctx.Err() == nil {
			if p = reg.onlyOne(); p != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if p == nil {
			t.Errorf("pending never registered")
			return
		}
		p.answerCh <- askResult{answer: "Yes", via: "button"}
	}()

	handleAsk(w, r, tg, store, limiter, reg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Answer string `json:"answer"`
		Via    string `json:"via"`
		Ms     int64  `json:"ms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Answer != "Yes" || resp.Via != "button" {
		t.Fatalf("resp: %+v", resp)
	}
	if reg.count() != 0 {
		t.Fatalf("registry not cleaned")
	}
}
```

- [ ] **Step 4: Run tests to verify failure**

Run: `go test ./... -run HandleAsk`
Expected: FAIL — `handleAsk` undefined.

- [ ] **Step 5: Implement `handleAsk` in `main.go`**

Place near `handleNotify`:

```go
func handleAsk(w http.ResponseWriter, r *http.Request, tg tgSender, store *Store, limiter *bucket, reg *pendingRegistry) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"GET or POST only"}`, http.StatusMethodNotAllowed)
		return
	}

	req, err := parseAskRequest(r)
	if err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, `{"error":"text is required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Buttons) > 6 {
		http.Error(w, `{"error":"too many buttons","max":6}`, http.StatusBadRequest)
		return
	}

	if !limiter.allow() {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded","limit":"60/min"}`))
		return
	}

	// concurrency cap before any Telegram call
	if reg.count() >= 5 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"too many concurrent questions","limit":5}`))
		return
	}

	timeoutDur := clampTimeout(req.Timeout)

	body := Notification{Text: req.Text, Title: req.Title, Level: req.Level, Service: req.Service}.Format()
	if len(req.Buttons) == 0 {
		body += "\n\n<i>(Reply to this message or type your answer)</i>"
	}

	subs := store.List()
	if len(subs) == 0 {
		http.Error(w, `{"error":"no subscribers"}`, http.StatusServiceUnavailable)
		return
	}

	// Send to first subscriber, register, fan out to the rest with the same body.
	// Reply-to / callback only resolves if the user uses the FIRST subscriber's message.
	// In the single-subscriber world this is effectively "the only message".
	first := subs[0]
	msgID, err := tg.SendWithKeyboard(first, body, req.Buttons)
	if err != nil {
		log.Printf("ask: send failed for %d: %v", first, err)
		http.Error(w, `{"error":"send failed"}`, http.StatusBadGateway)
		return
	}

	p := &pendingQ{
		id:       newAskID(),
		msgID:    msgID,
		answerCh: make(chan askResult, 1),
		deadline: time.Now().Add(timeoutDur),
	}
	reg.add(p)
	defer reg.remove(p)

	for _, chatID := range subs[1:] {
		if _, err := tg.SendWithKeyboard(chatID, body, req.Buttons); err != nil {
			log.Printf("ask: extra-send failed for %d: %v", chatID, err)
		}
	}

	start := time.Now()
	timer := time.NewTimer(timeoutDur)
	defer timer.Stop()

	select {
	case res := <-p.answerCh:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"answer": res.answer,
			"via":    res.via,
			"ms":     time.Since(start).Milliseconds(),
		})
	case <-timer.C:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "timeout",
			"ms":    timeoutDur.Milliseconds(),
		})
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./... -run HandleAsk` — all 5 must pass. Note `TestHandleAsk_TimeoutPath` takes ~5 seconds.
Run: `go test ./...` — full suite.

- [ ] **Step 7: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat: handleAsk endpoint with long-poll, concurrency cap, timeout"
```

---

## Task 6: Wire `/ask` route into `main()`

**Files:**
- Modify: `main.go` (the `main()` function, around the existing mux setup)

- [ ] **Step 1: Add the global registry**

After `notifyLimiter := newBucket(60, time.Second)` in `main()`:

```go
askReg := newPendingRegistry()
```

- [ ] **Step 2: Register the route**

After the existing `mux.HandleFunc("/notify", ...)` block:

```go
mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
	handleAsk(w, r, tg, store, notifyLimiter, askReg)
})
```

- [ ] **Step 3: Pass `askReg` to `pollBot`**

Change `go pollBot(tg, store)` → `go pollBot(tg, store, askReg)`.

- [ ] **Step 4: Update `pollBot` signature**

Change `func pollBot(tg *TG, store *Store)` → `func pollBot(tg *TG, store *Store, asks *pendingRegistry)` (the body is updated in tasks 7–9).

- [ ] **Step 5: Build**

Run: `go build ./...` — must compile (the registry is unused in pollBot for now; tasks 7–9 fill it).

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat: wire /ask route, pass askReg to pollBot"
```

---

## Task 7: Extend pollBot — callback_query branch

**Files:**
- Modify: `main.go` (top of the per-update loop in `pollBot`)
- Modify: `ask_test.go`

- [ ] **Step 1: Write failing test for the routing logic**

Append to `ask_test.go`:

```go
func TestRouteCallback_Resolves(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	p := &pendingQ{id: "x", msgID: 1234, answerCh: make(chan askResult, 1)}
	reg.add(p)

	cq := &CallbackQuery{
		ID:   "cq1",
		Data: "Yes",
		Message: &CallbackMessage{
			MessageID: 1234,
			Chat:      Chat{ID: 42},
		},
	}
	routeCallback(tg, reg, cq)

	select {
	case res := <-p.answerCh:
		if res.answer != "Yes" || res.via != "button" {
			t.Fatalf("res = %+v", res)
		}
	default:
		t.Fatal("answer channel was not resolved")
	}
	if len(tg.answerAcks) != 1 || tg.answerAcks[0] != "cq1" {
		t.Fatalf("answerAcks = %v", tg.answerAcks)
	}
	if _, ok := tg.editFinals[1234]; !ok {
		t.Fatal("EditMessageRemoveKeyboard not called")
	}
}

func TestRouteCallback_StaleAcksOnly(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()

	// No matching pending question
	cq := &CallbackQuery{
		ID:      "cq-stale",
		Data:    "X",
		Message: &CallbackMessage{MessageID: 9999, Chat: Chat{ID: 42}},
	}
	routeCallback(tg, reg, cq)

	if len(tg.answerAcks) != 1 {
		t.Fatalf("stale callback should still be acked, got %v", tg.answerAcks)
	}
	if len(tg.editFinals) != 0 {
		t.Fatal("stale callback should not edit any message")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run RouteCallback`
Expected: FAIL — `routeCallback` undefined.

- [ ] **Step 3: Implement `routeCallback` and wire into `pollBot`**

Add helper near `pollBot`:

```go
func routeCallback(tg tgSender, reg *pendingRegistry, cq *CallbackQuery) {
	if cq.Message == nil {
		_ = tg.AnswerCallbackQuery(cq.ID)
		return
	}
	p := reg.byMsg(cq.Message.MessageID)
	_ = tg.AnswerCallbackQuery(cq.ID)
	if p == nil {
		return // stale button press
	}
	select {
	case p.answerCh <- askResult{answer: cq.Data, via: "button"}:
	default:
	}
	_ = tg.EditMessageRemoveKeyboard(
		cq.Message.Chat.ID, cq.Message.MessageID,
		fmt.Sprintf("✅ Answered: %s", escapeHTML(cq.Data)),
	)
}
```

In `pollBot`, after `offset = u.UpdateID + 1` and before `if u.Message == nil { continue }`:

```go
if u.CallbackQuery != nil {
	routeCallback(tg, asks, u.CallbackQuery)
	continue
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./... -run RouteCallback`
Expected: PASS.
Run: `go test ./...`
Expected: full suite green.

- [ ] **Step 5: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat(pollBot): route callback_query to pending /ask"
```

---

## Task 8: Extend pollBot — reply-to routing

**Files:**
- Modify: `main.go`
- Modify: `ask_test.go`

- [ ] **Step 1: Write failing test**

Append to `ask_test.go`:

```go
func TestRouteReply_Resolves(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	p := &pendingQ{id: "x", msgID: 5555, answerCh: make(chan askResult, 1)}
	reg.add(p)

	msg := &Message{
		MessageID:      6000,
		Chat:           Chat{ID: 42},
		Text:           "my answer",
		ReplyToMessage: &ReplyMeta{MessageID: 5555},
	}
	if !routeReply(tg, reg, msg) {
		t.Fatal("routeReply returned false on a matching reply")
	}

	select {
	case res := <-p.answerCh:
		if res.answer != "my answer" || res.via != "reply" {
			t.Fatalf("res = %+v", res)
		}
	default:
		t.Fatal("answer channel was not resolved")
	}
	if _, ok := tg.editFinals[5555]; !ok {
		t.Fatal("EditMessageRemoveKeyboard not called for reply target")
	}
}

func TestRouteReply_NoMatch_FallsThrough(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()

	msg := &Message{
		MessageID:      6000,
		Chat:           Chat{ID: 42},
		Text:           "hello",
		ReplyToMessage: &ReplyMeta{MessageID: 9999},
	}
	if routeReply(tg, reg, msg) {
		t.Fatal("routeReply should return false when no pending matches")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run RouteReply`
Expected: FAIL — `routeReply` undefined.

- [ ] **Step 3: Implement and wire**

Add helper:

```go
// routeReply tries to resolve a pending /ask via reply-to. Returns true if
// the message was consumed by the routing layer (and pollBot should `continue`).
func routeReply(tg tgSender, reg *pendingRegistry, msg *Message) bool {
	if msg.ReplyToMessage == nil {
		return false
	}
	p := reg.byMsg(msg.ReplyToMessage.MessageID)
	if p == nil {
		return false
	}
	select {
	case p.answerCh <- askResult{answer: msg.Text, via: "reply"}:
	default:
	}
	_ = tg.EditMessageRemoveKeyboard(
		msg.Chat.ID, msg.ReplyToMessage.MessageID,
		fmt.Sprintf("✅ Answered: %s", escapeHTML(truncate(msg.Text, 80))),
	)
	return true
}
```

In `pollBot`, after `chatID := u.Message.Chat.ID` and `cmd := strings.TrimSpace(u.Message.Text)`, add **before** the existing `switch`:

```go
if routeReply(tg, asks, u.Message) {
	continue
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./...` — full suite green.

- [ ] **Step 5: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat(pollBot): route reply-to messages to pending /ask"
```

---

## Task 9: Extend pollBot — sequential text fallback

**Files:**
- Modify: `main.go`
- Modify: `ask_test.go`

- [ ] **Step 1: Write failing test**

Append to `ask_test.go`:

```go
func TestRouteSequential_Resolves(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	p := &pendingQ{id: "x", msgID: 7777, answerCh: make(chan askResult, 1)}
	reg.add(p)

	msg := &Message{MessageID: 8000, Chat: Chat{ID: 42}, Text: "yes please"}
	if !routeSequential(tg, reg, msg) {
		t.Fatal("routeSequential returned false on single-pending plain text")
	}

	select {
	case res := <-p.answerCh:
		if res.answer != "yes please" || res.via != "text" {
			t.Fatalf("res = %+v", res)
		}
	default:
		t.Fatal("not resolved")
	}
}

func TestRouteSequential_SkipsCommands(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	reg.add(&pendingQ{id: "x", msgID: 7777, answerCh: make(chan askResult, 1)})

	msg := &Message{MessageID: 8000, Chat: Chat{ID: 42}, Text: "/start"}
	if routeSequential(tg, reg, msg) {
		t.Fatal("slash commands should not be consumed by sequential fallback")
	}
}

func TestRouteSequential_SkipsWhenMultiplePending(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()
	reg.add(&pendingQ{id: "a", msgID: 1, answerCh: make(chan askResult, 1)})
	reg.add(&pendingQ{id: "b", msgID: 2, answerCh: make(chan askResult, 1)})

	msg := &Message{MessageID: 8000, Chat: Chat{ID: 42}, Text: "yes"}
	if routeSequential(tg, reg, msg) {
		t.Fatal("with 2 pending, plain text must not auto-resolve")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run RouteSequential`
Expected: FAIL.

- [ ] **Step 3: Implement and wire**

Add helper:

```go
func routeSequential(tg tgSender, reg *pendingRegistry, msg *Message) bool {
	if strings.HasPrefix(msg.Text, "/") {
		return false
	}
	p := reg.onlyOne()
	if p == nil {
		return false
	}
	select {
	case p.answerCh <- askResult{answer: msg.Text, via: "text"}:
	default:
	}
	_ = tg.EditMessageRemoveKeyboard(
		msg.Chat.ID, p.msgID,
		fmt.Sprintf("✅ Answered: %s", escapeHTML(truncate(msg.Text, 80))),
	)
	return true
}
```

In `pollBot`, after the `routeReply` call:

```go
if routeSequential(tg, asks, u.Message) {
	continue
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./...` — full suite green.

- [ ] **Step 5: Commit**

```bash
git add main.go ask_test.go
git commit -m "feat(pollBot): sequential text fallback for /ask"
```

---

## Task 10: Push to main, verify auto-deploy

**Files:**
- (none — git operations + monitoring)

- [ ] **Step 1: Push branch**

```bash
git push -u origin <branch-name>
```

- [ ] **Step 2: Open PR**

```bash
gh pr create --base main --head <branch-name> \
  --title "feat: /ask endpoint (long-poll Q&A via Telegram)" \
  --body "Spec: docs/superpowers/specs/2026-05-03-notify-bot-ask-endpoint-design.md"
```

- [ ] **Step 3: Squash-merge**

```bash
gh pr merge --squash --delete-branch <PR#>
```

- [ ] **Step 4: Wait for cron auto-deploy (~1 min) and verify**

```bash
ssh -p 2277 root@siestalenses.ru "tail -n 30 /var/log/notify-bot_deploy.log"
```

Expected: a recent block ending with `==> Deploy complete! Healthy.`.

- [ ] **Step 5: Verify `/notify` and `/health` still work**

```bash
curl -s https://notify.siestalenses.ru/health
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"text":"smoke after /ask deploy","level":"info"}' \
  https://notify.siestalenses.ru/notify
```

Both return 200 with JSON. **Do not test `/ask` yet** — proxy timeouts not bumped.

---

## Task 11: Bump HAProxy timeouts on the server

**Files (server-side, not in repo):**
- Modify: `/root/haproxy/haproxy.cfg`

- [ ] **Step 1: Backup current config**

```bash
ssh -p 2277 root@siestalenses.ru \
  "cp /root/haproxy/haproxy.cfg /root/haproxy/haproxy.cfg.bak.$(date +%Y%m%d-%H%M%S)"
```

- [ ] **Step 2: Read current timeouts**

```bash
ssh -p 2277 root@siestalenses.ru "grep -E 'timeout (client|server|connect)' /root/haproxy/haproxy.cfg"
```

If `timeout client` and `timeout server` are already ≥ 11m — skip steps 3 and 4, jump to validation in step 5.

- [ ] **Step 3: Edit `defaults` block**

Edit `/root/haproxy/haproxy.cfg` to set:

```haproxy
defaults
    timeout client  11m
    timeout server  11m
    timeout connect 5s
```

- [ ] **Step 4: Validate config**

```bash
ssh -p 2277 root@siestalenses.ru \
  "docker run --rm -v /root/haproxy/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro haproxy:latest haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg"
```

Expected output: `Configuration file is valid`.

- [ ] **Step 5: Restart HAProxy**

```bash
ssh -p 2277 root@siestalenses.ru "docker restart haproxy"
```

- [ ] **Step 6: Regression check**

```bash
curl -I https://siestalenses.ru
curl -I https://cloud.siestalenses.ru
curl -I https://notify.siestalenses.ru/health
```

All three must return 200 / 3xx, no connection errors.

---

## Task 12: Bump Caddy transport timeouts

**Files (server-side, not in repo):**
- Modify: `/root/caddy/Caddyfile`

- [ ] **Step 1: Backup**

```bash
ssh -p 2277 root@siestalenses.ru \
  "cp /root/caddy/Caddyfile /root/caddy/Caddyfile.bak.$(date +%Y%m%d-%H%M%S)"
```

- [ ] **Step 2: Edit the notify site block**

Replace:

```caddyfile
notify.siestalenses.ru {
    reverse_proxy notify-bot:9119
}
```

with:

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

- [ ] **Step 3: Validate**

```bash
ssh -p 2277 root@siestalenses.ru \
  "docker exec caddy caddy validate --config /etc/caddy/Caddyfile"
```

Expected: `Valid configuration`.

- [ ] **Step 4: Reload**

```bash
ssh -p 2277 root@siestalenses.ru \
  "docker exec caddy caddy reload --config /etc/caddy/Caddyfile"
```

- [ ] **Step 5: Verify**

```bash
curl -I https://notify.siestalenses.ru/health
```

Expected: 200.

---

## Task 13: Manual smoke tests against production

**Files:**
- (none — these are interactive smoke tests)

- [ ] **Step 1: POST free-form**

```bash
curl -s -X POST --max-time 310 'https://notify.siestalenses.ru/ask' \
  -H 'Content-Type: application/json' \
  -d '{"text":"Smoke test free-form. Reply or type anything.","timeout":300}'
```

In Telegram, reply to (or just type after) the bot's message. Expected response JSON: `{"answer":"<your text>","via":"reply"|"text","ms":<n>}`.

- [ ] **Step 2: POST with buttons**

```bash
curl -s -X POST --max-time 310 'https://notify.siestalenses.ru/ask' \
  -H 'Content-Type: application/json' \
  -d '{"text":"Pick one","timeout":300,"buttons":["Yes","No","Maybe"]}'
```

In Telegram, tap a button. Expected: `{"answer":"<choice>","via":"button","ms":<n>}` and the bot edits its message to `✅ Answered: <choice>`.

- [ ] **Step 3: GET fallback**

```bash
curl -sG --max-time 310 'https://notify.siestalenses.ru/ask' \
  --data-urlencode 'text=GET fallback test' \
  --data-urlencode 'timeout=60'
```

Reply something. Expected: same JSON as Step 1.

- [ ] **Step 4: 408 timeout path**

```bash
curl -s -X POST --max-time 70 'https://notify.siestalenses.ru/ask' \
  -H 'Content-Type: application/json' \
  -d '{"text":"Timeout test, do not reply","timeout":60}'
```

Don't reply. After ~60 seconds expect: HTTP 408, body `{"error":"timeout","ms":60000}`.

- [ ] **Step 5: Concurrency cap**

```bash
for i in 1 2 3 4 5 6; do
  curl -s -o "/tmp/ask-$i.json" -w "%{http_code}\n" \
    -X POST --max-time 35 'https://notify.siestalenses.ru/ask' \
    -H 'Content-Type: application/json' \
    -d "{\"text\":\"concurrency $i\",\"timeout\":30}" &
done
wait
cat /tmp/ask-*.json
```

Expected: at least one response with HTTP 429 and body `{"error":"too many concurrent questions","limit":5}`. The other five eventually 408 (or get answered if you reply quickly).

- [ ] **Step 6: Notify the user**

If all five smoke checks pass, post a status update via the existing notification path:

```bash
curl -s -X POST 'https://notify.siestalenses.ru/notify' \
  -H 'Content-Type: application/json' \
  -d '{"text":"/ask deployed and verified","level":"success","title":"Deploy","service":"notify-bot"}'
```

---

## Task 14: Create the `bot-mode` skill (modal)

**Files:**
- Create: `~/.claude/skills/bot-mode/SKILL.md`

This skill is **strictly modal**. There is no per-question invocation. The user enables a session-wide "bot mode" with one phrase; while that mode is on, every clarifying question Claude would otherwise ask in chat goes through `POST /ask`. Another phrase exits the mode.

- [ ] **Step 1: Write the skill**

Create the file with:

````markdown
---
name: bot-mode
description: Use ONLY when the user enables session-wide "bot mode" — meaning all subsequent clarifying questions Claude asks should go through Telegram via /ask, not the chat. Mode-on triggers (Russian or English) — "перейди в режим бота", "включи бот-мод", "общайся через бот", "режим телеги", "управляй через бот", "turn on bot mode", "switch to telegram mode". Mode-off triggers — "выключи режим бота", "обратно в чат", "stop bot mode". Do NOT use for one-off questions; the user has explicitly stated they want only the modal flow. Notifications still go via notify-via-bot.
---

# Ask via Bot — Modal

Session-level mode. While enabled, every clarifying question Claude would normally ask the user in chat is instead asked via `POST /ask` — a long-poll HTTP call that blocks until the user replies in Telegram (text, reply-to, or inline button) or hits the 5-minute timeout.

While mode is on:
- **Questions to user** → `/ask` (Telegram). Wait for reply, then continue.
- **Status updates, progress, final results, errors** → still chat.
- **Fire-and-forget notifications** → still `notify-via-bot` (`/notify`), unchanged.

There is **no per-question invocation** of this skill. If the user asks a one-off question via Telegram, that's not what this skill is for — they would have to enable bot mode first.

## How mode is tracked

The mode flag lives in the current conversation context. When you enable it (in response to a mode-on trigger), keep the flag set in your reasoning until:
- the user issues a mode-off trigger, OR
- the conversation ends.

There is no persistent file or memory entry — each new session starts in normal (chat) mode.

When you enable the mode, acknowledge briefly in chat:

> Бот-режим включён. Все мои уточняющие вопросы теперь будут уходить в Telegram через `@mvv_notify_bot`. Скажи «выключи режим бота», чтобы вернуться к чату.

When you disable it:

> Бот-режим выключен. Уточнения снова в чате.

If a question is already in flight (request still blocking on `/ask`) when the user types in chat to disable the mode — finish that request normally (or let it 408), then disable. Don't try to cancel mid-flight.

## Endpoint

`POST` is the only correct method from this skill. `GET` is a manual-shell fallback the user might use directly — never call it from here.

```
POST https://notify.siestalenses.ru/ask
Content-Type: application/json

{
  "text":    "<question>",
  "timeout": 300,
  "buttons": ["Yes", "No"],
  "level":   "info | success | warn | error | critical",
  "title":   "<optional>",
  "service": "<optional>"
}
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `text` | yes | — | The question body. Same formatting as `/notify`. |
| `timeout` | no | `300` | Seconds; clamped to `[5, 600]` server-side. |
| `buttons` | no | — | Up to 6 strings. Each becomes a Telegram inline button; label = `callback_data` = the same string. |
| `level` / `title` / `service` | no | — | Same as `/notify`. |

Successful response (HTTP 200):

```json
{"answer": "Yes", "via": "button|reply|text", "ms": 1843}
```

Errors:
- `400` — missing `text`, or > 6 buttons.
- `408` — timeout reached, no reply. Body: `{"error":"timeout","ms":300000}`.
- `429` `{"error":"too many concurrent questions","limit":5}` — wait briefly, retry once, or fall back to chat.
- `429` `{"error":"rate limit exceeded"}` — shared 60/min bucket with `/notify`. Same handling.
- `5xx` / connection drop — container may have just been rebuilt by auto-deploy. Fall back to chat, don't retry blindly.

## How to call (UTF-8 — IMPORTANT)

`bash` on Windows mangles literal non-ASCII inside `-d '...'` (Cyrillic gets replaced with `?`). For any text that isn't pure ASCII — Russian, emoji, accented chars, CJK — **build the JSON via the Write tool to a temp file**, then `curl --data-binary @file`.

Procedure for non-ASCII (default for this skill since the user is Russian-speaking):

1. Use the **Write tool** to create the JSON file. E.g. `/tmp/ask.json` with the literal JSON body. Write preserves UTF-8.

2. Send:

```bash
curl -s -X POST --max-time 310 'https://notify.siestalenses.ru/ask' \
  -H 'Content-Type: application/json; charset=utf-8' \
  --data-binary @/tmp/ask.json
```

`--max-time` must be ≥ body `timeout` + 10 seconds so curl doesn't drop the connection before the server's 408 response.

For pure-ASCII questions inline `-d '...'` works:

```bash
curl -s -X POST --max-time 310 'https://notify.siestalenses.ru/ask' \
  -H 'Content-Type: application/json' \
  -d '{"text":"Continue with deleting 12 stale branches?","timeout":300,"buttons":["Yes","No"]}'
```

When unsure — default to the file approach.

## When to use buttons vs free-form

- **Use buttons** for closed questions: yes/no, pick-one-of-N, decision points where the answer is a known label.
- **Free-form** (omit `buttons`) for open-ended: "what name for this branch?", "where do you want this saved?".

Prefer 2–4 buttons; 6 is the hard max.

## Choosing the timeout

- Default (300s = 5 min) — fits "user is at keyboard, will see push soon".
- Lower (60–120s) — only when you'd rather fail fast and pivot to chat.
- Higher (up to 600s) — only when you genuinely expect the user is away.

Don't chain back-to-back `/ask` calls to "extend" the wait — that just floods Telegram.

## Phrasing the question

- ONE question per call. Don't stuff multiple decisions into one `text`.
- Be concrete. Bad: «Продолжить?». Good: «Удалить 12 устаревших веток с remote? Отменить нельзя».
- For irreversible actions, echo what's about to happen so the user has full context inside Telegram (the chat context isn't visible there).
- Keep it short — Telegram crops past ~4000 chars and you don't want to bury the question.

## Reporting back in chat

After `/ask` returns, briefly tell the user in chat what came back, then continue:

> Из Telegram: «Yes» — удаляю.

On 408:

> Не дождался ответа в Telegram (5 мин). Возвращаю вопрос в чат: <re-ask the question here>?

On 429 (concurrency): wait ~3s and retry once. Still 429 → fall back to chat.

On 5xx / connection drop: assume notify-bot was restarted; fall back to chat, no auto-retry.

## Sensitive content

Question text lands in Telegram chat history. Don't put secrets/tokens in `text`. POST keeps the body out of HAProxy/Caddy access logs (that's why we use POST), but Telegram itself stores the message.
````

- [ ] **Step 2: Verify file exists**

```bash
ls -la ~/.claude/skills/bot-mode/SKILL.md
```

The skill auto-loads in new Claude Code sessions; no further action.

---

## Final verification

After all tasks:

- [ ] Run `go test ./...` locally — all green.
- [ ] PR merged, auto-deploy log shows healthy deploy.
- [ ] All 5 smoke tests in Task 13 passed.
- [ ] `/notify` regression test passed in Task 13 Step 6.
- [ ] Server timeouts confirmed via `grep` in Task 11.
- [ ] Skill file present in `~/.claude/skills/bot-mode/SKILL.md`.

If any verification fails — see the Rollback table in the spec.
