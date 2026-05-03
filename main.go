package main

import (
	"bytes"
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

func (s *Store) save() {
	ids := s.List()
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

	log.Printf("notify-bot starting on :%d (%d subscribers)", cfg.HTTPPort, store.Count())

	// bot polling goroutine
	go pollBot(tg, store)

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
		handleNotify(w, r, tg, store)
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

func pollBot(tg *TG, store *Store) {
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
			if u.Message == nil {
				continue
			}
			chatID := u.Message.Chat.ID
			cmd := strings.TrimSpace(u.Message.Text)

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

func handleNotify(w http.ResponseWriter, r *http.Request, tg *TG, store *Store) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}

	var n Notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
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
