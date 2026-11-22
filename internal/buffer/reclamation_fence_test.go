package buffer

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// Exhaust the shared RAM budget with another buffer so the target uses the
// fully-present fast disk path, rather than reading from resident memory.
func newReclamationTestBuffer(t *testing.T) (*Buffer, []byte) {
	t.Helper()
	pool := NewPool(PoolConfig{Name: "reclamation-fence", MemoryBudget: blockSize})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	holder, err := pool.NewBuffer(Config{TotalSize: blockSize})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	b, err := pool.NewBuffer(Config{TotalSize: blockSize})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x5a}, blockSize)
	if _, err := b.WriteAt(want, 0); err != nil {
		t.Fatal(err)
	}
	if slot := b.stateSlot(0); slot == nil || slot.Load() != stateFastDisk {
		t.Fatal("fixture did not reach the fast disk path")
	}
	return b, want
}

func TestReclamationWaitsForFastDiskRead(t *testing.T) {
	b, want := newReclamationTestBuffer(t)
	readStarted, releaseRead, punchStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseRead) }) }
	defer release()
	b.beforeFastDiskReadForTest = func() {
		close(readStarted)
		<-releaseRead
	}
	b.beforeDiscardPunchForTest = func() { close(punchStarted) }

	got := make([]byte, len(want))
	readDone := make(chan error, 1)
	go func() { _, err := b.ReadAt(got, 0); readDone <- err }()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("fast disk read did not start")
	}
	discardDone := make(chan error, 1)
	go func() { discardDone <- b.Discard(0, blockSize) }()
	select {
	case <-punchStarted:
		t.Fatal("discard overtook a fast disk read after its presence check")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-readDone; err != nil || !bytes.Equal(got, want) {
		t.Fatalf("in-flight read lost its original data: %v", err)
	}
	if err := <-discardDone; err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadAt(got, 0); !errors.Is(err, ErrNotPresent) {
		t.Fatalf("discarded bytes still reported present: %v", err)
	}
}

func TestRefillWaitsForPhysicalReclamation(t *testing.T) {
	b, _ := newReclamationTestBuffer(t)
	punchStarted, releasePunch := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releasePunch) }) }
	defer release()
	b.beforeDiscardPunchForTest = func() {
		close(punchStarted)
		<-releasePunch
		// Windows does not implement punchHole. Overwrite the old range
		// here to model its destructive effect on every test platform.
		if _, err := b.file.WriteAt(make([]byte, blockSize), 0); err != nil {
			t.Errorf("simulate physical hole punch: %v", err)
		}
	}

	discardDone := make(chan error, 1)
	go func() { discardDone <- b.Discard(0, blockSize) }()
	select {
	case <-punchStarted:
	case <-time.After(time.Second):
		t.Fatal("discard did not reach the physical reclamation boundary")
	}
	want := bytes.Repeat([]byte{0x31}, blockSize)
	writeStarted, writeDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(writeStarted)
		_, err := b.WriteAt(want, 0)
		writeDone <- err
	}()
	<-writeStarted
	select {
	case err := <-writeDone:
		t.Fatalf("refill completed before the old disk range was punched: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-discardDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := b.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("refill was lost after reclaim: %v", err)
	}
}
