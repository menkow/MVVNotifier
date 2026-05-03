package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestPendingQRegistry_TryReserve(t *testing.T) {
	reg := newPendingRegistry()
	for i := 0; i < 5; i++ {
		if !reg.tryReserve() {
			t.Fatalf("tryReserve %d should succeed", i+1)
		}
	}
	if reg.tryReserve() {
		t.Fatal("tryReserve 6th should fail (cap=5)")
	}
	reg.release()
	if !reg.tryReserve() {
		t.Fatal("tryReserve after release should succeed")
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
	if got.Timeout != 0 {
		t.Fatalf("Timeout = %d, want 0", got.Timeout)
	}
}

// fakeTG records calls and lets the test resolve pending questions externally.
type fakeTG struct {
	mu         sync.Mutex
	sendCalls  int
	nextMsgID  int64
	answerAcks []string
	editCalls  map[int64]bool
}

func newFakeTG() *fakeTG {
	return &fakeTG{nextMsgID: 1000, editCalls: make(map[int64]bool)}
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

func (f *fakeTG) EditMessageWithAnswer(chatID, messageID int64, body, answer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editCalls[messageID] = true
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
	for i := 0; i < 5; i++ {
		if !reg.tryReserve() {
			t.Fatalf("tryReserve unexpectedly false at i=%d", i)
		}
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

	go func() {
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
	if _, ok := tg.editCalls[1234]; !ok {
		t.Fatal("EditMessageRemoveKeyboard not called")
	}
}

func TestRouteCallback_StaleAcksOnly(t *testing.T) {
	reg := newPendingRegistry()
	tg := newFakeTG()

	cq := &CallbackQuery{
		ID:      "cq-stale",
		Data:    "X",
		Message: &CallbackMessage{MessageID: 9999, Chat: Chat{ID: 42}},
	}
	routeCallback(tg, reg, cq)

	if len(tg.answerAcks) != 1 {
		t.Fatalf("stale callback should still be acked, got %v", tg.answerAcks)
	}
	if len(tg.editCalls) != 0 {
		t.Fatal("stale callback should not edit any message")
	}
}

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
	if _, ok := tg.editCalls[5555]; !ok {
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
