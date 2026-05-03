package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Config ---

type Config struct {
	BotToken string `json:"bot_token"`
	HTTPPort int    `json:"http_port"`
}

func loadConfig() Config {
	cfg := Config{HTTPPort: 9119}

	// env vars take priority
	if t := os.Getenv("NOTIFY_BOT_TOKEN"); t != "" {
		cfg.BotToken = t
	}
	if p := os.Getenv("NOTIFY_HTTP_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			cfg.HTTPPort = v
		}
	}

	// try config file
	for _, path := range []string{"config.json", "/etc/notify-bot/config.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var fc Config
		if json.Unmarshal(data, &fc) == nil {
			if cfg.BotToken == "" && fc.BotToken != "" {
				cfg.BotToken = fc.BotToken
			}
			if fc.HTTPPort != 0 {
				cfg.HTTPPort = fc.HTTPPort
			}
		}
		break
	}

	if cfg.BotToken == "" {
		log.Fatal("bot token not set: use NOTIFY_BOT_TOKEN env or config.json")
	}
	return cfg
}

// --- Subscriber Store ---

type Store struct {
	mu   sync.RWMutex
	subs map[int64]bool
	path string
}

func NewStore(path string) *Store {
	s := &Store{subs: make(map[int64]bool), path: path}
	s.load()
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var ids []int64
	if json.Unmarshal(data, &ids) == nil {
		for _, id := range ids {
			s.subs[id] = true
		}
	}
}

// save assumes the caller already holds s.mu. It must not call any other
// Store method that locks, otherwise sync.RWMutex (non-reentrant) deadlocks.
func (s *Store) save() {
	ids := make([]int64, 0, len(s.subs))
	for id := range s.subs {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	os.WriteFile(s.path, data, 0644)
}

func (s *Store) Add(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs[chatID] {
		return false
	}
	s.subs[chatID] = true
	s.save()
	return true
}

func (s *Store) Remove(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.subs[chatID] {
		return false
	}
	delete(s.subs, chatID)
	s.save()
	return true
}

func (s *Store) List() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]int64, 0, len(s.subs))
	for id := range s.subs {
		ids = append(ids, id)
	}
	return ids
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// --- Telegram API ---

type TG struct {
	token  string
	client *http.Client
}

func NewTG(token string) *TG {
	return &TG{
		token:  token,
		client: &http.Client{Timeout: 35 * time.Second},
	}
}

func (t *TG) api(method string, payload any) (json.RawMessage, error) {
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", t.token, method)
	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var result struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Desc   string          `json:"description"`
	}
	json.Unmarshal(data, &result)
	if !result.OK {
		return nil, fmt.Errorf("telegram: %s", result.Desc)
	}
	return result.Result, nil
}

func (t *TG) Send(chatID int64, text string) error {
	_, err := t.api("sendMessage", map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	return err
}

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

// AnswerCallbackQuery acknowledges a button press so Telegram stops the loading
// spinner. Telegram requires this within 30 seconds.
func (t *TG) AnswerCallbackQuery(callbackID string) error {
	_, err := t.api("answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
	})
	return err
}

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

// tgSender is the subset of TG operations needed by handleAsk. It exists so
// tests can supply a fake without hitting the real Telegram API. *TG already
// satisfies it through the methods above.
type tgSender interface {
	SendWithKeyboard(chatID int64, text string, buttons []string) (int64, error)
	AnswerCallbackQuery(callbackID string) error
	EditMessageRemoveKeyboard(chatID, messageID int64, finalText string) error
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID      int64      `json:"message_id"`
	Chat           Chat       `json:"chat"`
	Text           string     `json:"text"`
	ReplyToMessage *ReplyMeta `json:"reply_to_message"`
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

func (t *TG) GetUpdates(offset int64) ([]Update, error) {
	raw, err := t.api("getUpdates", map[string]any{
		"offset":  offset,
		"timeout": 30,
	})
	if err != nil {
		return nil, err
	}
	var updates []Update
	json.Unmarshal(raw, &updates)
	return updates, nil
}

// --- Notification Formatting ---

var levelEmoji = map[string]string{
	"info":     "\u2139\ufe0f",
	"success":  "\u2705",
	"warn":     "\u26a0\ufe0f",
	"warning":  "\u26a0\ufe0f",
	"error":    "\u274c",
	"critical": "\U0001f6a8",
}

type Notification struct {
	Text    string `json:"text"`
	Title   string `json:"title,omitempty"`
	Level   string `json:"level,omitempty"`
	Service string `json:"service,omitempty"`
}

func (n Notification) Format() string {
	level := strings.ToLower(n.Level)
	if level == "" {
		level = "info"
	}
	emoji := levelEmoji[level]
	if emoji == "" {
		emoji = levelEmoji["info"]
	}

	var b strings.Builder

	// header
	if n.Title != "" {
		b.WriteString(fmt.Sprintf("%s <b>%s</b>\n", emoji, escapeHTML(n.Title)))
	} else {
		b.WriteString(fmt.Sprintf("%s <b>%s</b>\n", emoji, strings.ToUpper(level)))
	}

	// service tag
	if n.Service != "" {
		b.WriteString(fmt.Sprintf("<code>[%s]</code> ", escapeHTML(n.Service)))
	}

	b.WriteString(escapeHTML(n.Text))
	return b.String()
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// --- Rate Limiter ---

type bucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

// newBucket builds a token bucket. perToken is how long it takes to
// regenerate one token. e.g. (60, time.Second) → capacity 60, 1 token/sec.
func newBucket(capacity int, perToken time.Duration) *bucket {
	return &bucket{
		tokens:     float64(capacity),
		capacity:   float64(capacity),
		refillRate: 1.0 / perToken.Seconds(),
		lastRefill: time.Now(),
	}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillRate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastRefill = now
	}

	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// --- Ask Helpers ---

func newAskID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func clampTimeout(t int) time.Duration {
	if t < 5 {
		t = 300 // default when missing/invalid
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

// --- Ask Request ---

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
	mu    sync.Mutex
	id2q  map[string]*pendingQ
	msg2q map[int64]*pendingQ
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

// --- Main ---

func main() {
	// CLI mode: notify-bot send "message" [--level X] [--title Y] [--service Z]
	if len(os.Args) > 1 && os.Args[1] == "send" {
		cliSend()
		return
	}

	cfg := loadConfig()
	tg := NewTG(cfg.BotToken)
	store := NewStore("subscribers.json")
	notifyLimiter := newBucket(60, time.Second) // 60 req/min global, lazy refill
	askReg := newPendingRegistry()

	log.Printf("notify-bot starting on :%d (%d subscribers)", cfg.HTTPPort, store.Count())

	// bot polling goroutine
	go pollBot(tg, store, askReg)

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(w, r, tg, store, notifyLimiter)
	})
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		handleAsk(w, r, tg, store, notifyLimiter, askReg)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"subscribers": store.Count(),
		})
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: mux,
	}

	// graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down...")
		server.Close()
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

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

// routeSequential consumes a plain text message into the sole pending /ask
// (if there is exactly one). Slash commands always fall through to the
// existing /start, /stop, /help, /status handlers.
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

func pollBot(tg *TG, store *Store, asks *pendingRegistry) {
	var offset int64
	for {
		updates, err := tg.GetUpdates(offset)
		if err != nil {
			log.Printf("poll error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.CallbackQuery != nil {
				routeCallback(tg, asks, u.CallbackQuery)
				continue
			}
			if u.Message == nil {
				continue
			}
			chatID := u.Message.Chat.ID
			cmd := strings.TrimSpace(u.Message.Text)

			if routeReply(tg, asks, u.Message) {
				continue
			}

			if routeSequential(tg, asks, u.Message) {
				continue
			}

			switch {
			case cmd == "/start":
				if store.Add(chatID) {
					tg.Send(chatID, "\u2705 <b>Subscribed!</b>\nYou will receive notifications from this bot.\n\n/stop — unsubscribe\n/status — check status")
					log.Printf("subscriber added: %d (%s)", chatID, u.Message.Chat.Username)
				} else {
					tg.Send(chatID, "You are already subscribed.\n\n/stop — unsubscribe")
				}

			case cmd == "/stop":
				if store.Remove(chatID) {
					tg.Send(chatID, "\U0001f6d1 <b>Unsubscribed.</b>\nYou will no longer receive notifications.\n\n/start — subscribe again")
					log.Printf("subscriber removed: %d", chatID)
				} else {
					tg.Send(chatID, "You are not subscribed.\n\n/start — subscribe")
				}

			case cmd == "/status":
				tg.Send(chatID, fmt.Sprintf("\u2139\ufe0f You are subscribed.\nTotal subscribers: %d", store.Count()))

			case cmd == "/help":
				tg.Send(chatID, "<b>Notification Bot</b>\n\n/start — subscribe to notifications\n/stop — unsubscribe\n/status — check status\n/help — this message")
			}
		}
	}
}

func handleNotify(w http.ResponseWriter, r *http.Request, tg *TG, store *Store, limiter *bucket) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"GET or POST only"}`, http.StatusMethodNotAllowed)
		return
	}

	if !limiter.allow() {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded","limit":"60/min"}`))
		return
	}

	var n Notification
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
	} else {
		q := r.URL.Query()
		n.Text = q.Get("text")
		n.Title = q.Get("title")
		n.Level = q.Get("level")
		n.Service = q.Get("service")
	}
	if n.Text == "" {
		http.Error(w, `{"error":"text is required"}`, http.StatusBadRequest)
		return
	}

	msg := n.Format()
	subs := store.List()
	sent, failed := 0, 0

	for _, chatID := range subs {
		if err := tg.Send(chatID, msg); err != nil {
			log.Printf("send error to %d: %v", chatID, err)
			failed++
		} else {
			sent++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"sent":   sent,
		"failed": failed,
		"total":  len(subs),
	})
}

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

// --- CLI Send Mode ---

func cliSend() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: notify-bot send \"message\" [--level info|warn|error|critical] [--title Title] [--service name]")
		os.Exit(1)
	}

	n := Notification{Text: os.Args[2]}
	for i := 3; i < len(os.Args)-1; i += 2 {
		switch os.Args[i] {
		case "--level", "-l":
			n.Level = os.Args[i+1]
		case "--title", "-t":
			n.Title = os.Args[i+1]
		case "--service", "-s":
			n.Service = os.Args[i+1]
		}
	}

	port := os.Getenv("NOTIFY_HTTP_PORT")
	if port == "" {
		port = "9119"
	}

	body, _ := json.Marshal(n)
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%s/notify", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n(is notify-bot running?)\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
}
