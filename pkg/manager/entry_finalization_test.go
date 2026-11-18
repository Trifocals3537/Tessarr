package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/pkg/storage"
)

func waitForFinalizationWork(t *testing.T, lifecycle *entryLifecycle, key string, count int) {
	t.Helper()
	state, err := lifecycle.stateFor(key, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		n := len(state.active)
		state.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("entry %q did not have %d registered workers", key, count)
}

func TestEntryFinalizationWaitIsCancelableAndOtherEntriesRemainIndependent(t *testing.T) {
	lifecycle := newEntryLifecycle()
	entry, other := &storage.Entry{InfoHash: "one"}, &storage.Entry{InfoHash: "two"}
	for _, e := range []*storage.Entry{entry, other} {
		if err := lifecycle.bindEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := lifecycle.startFinalization(context.Background(), entry.InfoHash, entry.QueueGeneration)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		lease, err := lifecycle.startFinalization(ctx, entry.InfoHash, entry.QueueGeneration)
		if lease != nil {
			lease.Close()
		}
		waiter <- err
	}()
	waitForFinalizationWork(t, lifecycle, entry.InfoHash, 2)
	independent, err := lifecycle.startFinalization(ctx, other.InfoHash, other.QueueGeneration)
	if err != nil {
		t.Fatal("an unrelated entry was blocked", err)
	}
	independent.Close()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("waiting finalizer ignored cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting finalizer did not return")
	}
	waitForFinalizationWork(t, lifecycle, entry.InfoHash, 1)
	if owner.Context().Err() != nil {
		t.Fatal("canceling a waiter canceled its owner")
	}
}

func TestEntryFinalizationDeletionDrainsOwnerAndWaiterBeforeReplacement(t *testing.T) {
	lifecycle := newEntryLifecycle()
	entry := &storage.Entry{InfoHash: "replace"}
	if err := lifecycle.bindEntry(entry); err != nil {
		t.Fatal(err)
	}
	owner, err := lifecycle.startFinalization(context.Background(), entry.InfoHash, entry.QueueGeneration)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	waiter := make(chan error, 1)
	go func() {
		lease, err := lifecycle.startFinalization(context.Background(), entry.InfoHash, entry.QueueGeneration)
		if lease != nil {
			lease.Close()
		}
		waiter <- err
	}()
	waitForFinalizationWork(t, lifecycle, entry.InfoHash, 2)
	deletion, err := lifecycle.beginDelete(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Context().Err() == nil {
		t.Fatal("deletion did not cancel the owner")
	}
	owner.Close()
	if err := <-waiter; err == nil {
		t.Fatal("waiting finalizer was allowed to start during deletion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := deletion.Wait(ctx); err != nil {
		t.Fatal("deletion did not drain both workers", err)
	}
	deletion.Finish(true)
	if _, err := lifecycle.startFinalization(ctx, entry.InfoHash, entry.QueueGeneration); !errors.Is(err, ErrStaleQueueGeneration) {
		t.Fatal("old generation acquired replacement ownership", err)
	}
	replacement := &storage.Entry{InfoHash: entry.InfoHash}
	if err := lifecycle.bindEntry(replacement); err != nil {
		t.Fatal(err)
	}
	lease, err := lifecycle.startFinalization(ctx, replacement.InfoHash, replacement.QueueGeneration)
	if err != nil {
		t.Fatal("replacement could not acquire its own lease", err)
	}
	lease.Close()
	waitForFinalizationWork(t, lifecycle, entry.InfoHash, 0)
}
