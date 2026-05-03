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
