package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
