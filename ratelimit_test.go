package main

import (
	"testing"
	"time"
)

func TestBucketAllowsBurstUpToCapacity(t *testing.T) {
	b := newBucket(60, time.Second) // capacity 60, refill 1 token per 1s
	for i := 0; i < 60; i++ {
		if !b.allow() {
			t.Fatalf("expected allow on attempt %d, got reject", i+1)
		}
	}
	if b.allow() {
		t.Fatalf("expected reject on 61st attempt")
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	b := newBucket(60, time.Second)
	// drain
	for i := 0; i < 60; i++ {
		b.allow()
	}
	if b.allow() {
		t.Fatalf("expected reject after drain")
	}

	// simulate 2 seconds passing by rewinding the bucket's last-refill anchor
	b.lastRefill = b.lastRefill.Add(-2 * time.Second)

	if !b.allow() {
		t.Fatalf("expected allow after 2s of refill (2 tokens replenished)")
	}
	if !b.allow() {
		t.Fatalf("expected allow on second attempt after 2s of refill")
	}
	if b.allow() {
		t.Fatalf("expected reject on third attempt — only 2 tokens were refilled")
	}
}

func TestBucketDoesNotExceedCapacity(t *testing.T) {
	b := newBucket(60, time.Second)
	// pretend a long time passed without any traffic
	b.lastRefill = b.lastRefill.Add(-1 * time.Hour)

	// even with 1 hour of refill, capacity caps at 60
	for i := 0; i < 60; i++ {
		if !b.allow() {
			t.Fatalf("expected allow on attempt %d", i+1)
		}
	}
	if b.allow() {
		t.Fatalf("expected reject on 61st attempt — bucket should not exceed capacity")
	}
}
