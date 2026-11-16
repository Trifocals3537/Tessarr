package nntp

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundReservationsLeaveForegroundCapacity(t *testing.T) {
	pool := &ProviderPool{slots: make(chan struct{}, 2)}
	background := WithBackground(t.Context())
	if !trySlot(background, pool) {
		t.Fatal("idle background slot refused")
	}
	if trySlot(background, pool) {
		t.Fatal("background consumed foreground reservation")
	}
	if !trySlot(t.Context(), pool) {
		t.Fatal("foreground could not use reserved slot")
	}
	ctx, cancel := context.WithTimeout(background, 20*time.Millisecond)
	defer cancel()
	if err := waitSlot(ctx, pool); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait cancellation: %v", err)
	}
	if len(pool.slots) != 2 {
		t.Fatal("cancelled waiter leaked a slot")
	}
	<-pool.slots
	<-pool.slots
	if err := waitSlot(background, pool); err != nil {
		t.Fatal(err)
	}
	<-pool.slots
}

func TestSingleConnectionBackgroundRemainsBounded(t *testing.T) {
	pool := &ProviderPool{slots: make(chan struct{}, 1)}
	if !trySlot(WithBackground(t.Context()), pool) {
		t.Fatal("idle one-slot provider unavailable")
	}
	if trySlot(WithBackground(t.Context()), pool) || trySlot(t.Context(), pool) {
		t.Fatal("provider capacity exceeded")
	}
	<-pool.slots
}
