package reader

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"testing"

	appConfig "github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/nntp"
)

func TestArticleFailureObservationClaimIsPerFetch(t *testing.T) {
	cause := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430}
	first := &articleFailureObservation{cause: cause}
	second := &articleFailureObservation{cause: cause}
	if !ClaimArticleFailureObservation(first) || !ClaimArticleFailureObservation(second) ||
		ClaimArticleFailureObservation(&SegmentReadFailure{Offset: 16, cause: first}) {
		t.Fatal("claim must distinguish actual fetches and recognize delayed wrappers")
	}
	shared := &articleFailureObservation{cause: cause}
	claims := make(chan bool, 16)
	for range cap(claims) {
		go func() { claims <- ClaimArticleFailureObservation(shared) }()
	}
	var claimed int
	for range cap(claims) {
		if <-claims {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("shared fetch was claimed %d times, want one", claimed)
	}
	for range 2 {
		if !ClaimArticleFailureObservation(cause) {
			t.Fatal("untagged errors have no shared fetch identity")
		}
	}
}

func TestRecoveryProbeRefreshesCiphertextAndIVOnly(t *testing.T) {
	appConfig.SetConfigPath(t.TempDir())
	plain := bytes.Repeat([]byte{1, 2, 3, 4}, 16)
	stored := append([]byte(nil), plain...)
	key, iv := bytes.Repeat([]byte{0x42}, 32), bytes.Repeat([]byte{0x23}, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(stored, stored)

	segments := make([]SegmentMeta, 4)
	for i := range segments {
		segments[i] = SegmentMeta{Number: i + 1, MessageID: "fixture", Bytes: 16, StartOffset: int64(i * 16), EndOffset: int64(i*16 + 15)}
	}
	var refreshed []int
	r, err := NewStreamingReaderWithEncryption(t.Context(), &nntp.Client{}, segments,
		EncryptionConfig{Enabled: true, Key: key, IV: iv}, WithDiskPath(t.TempDir()),
		WithMaxConnections(1), WithPrefetchAhead(3), WithRecoveredArticle(func(_ context.Context, seg SegmentMeta, w io.Writer) (bool, error) {
			refreshed = append(refreshed, seg.Number)
			_, err := w.Write(stored[seg.StartOffset : seg.EndOffset+1])
			return true, err
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	cause := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430}
	// Ciphertext and its preceding IV block are failed; a healthy and a
	// different failed segment must not be reset by this one-byte probe.
	for _, i := range []int{0, 1, 3} {
		r.cache.MarkFailed(i, cause)
	}
	if err := r.cache.Put(2, stored[32:48]); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := r.ReadAt(one[:], 17); !errors.Is(err, cause) || len(refreshed) != 0 {
		t.Fatalf("normal read retried a permanent cache failure: %v, refreshed=%v", err, refreshed)
	}
	n, err := r.ReadAtContext(WithRecoveryProbe(t.Context()), one[:], 17)
	if err != nil || n != 1 || one[0] != plain[17] || len(refreshed) != 2 || refreshed[0] != 2 || refreshed[1] != 1 {
		t.Fatalf("probe = (%d,%v,%v), refreshed=%v", n, err, one, refreshed)
	}
	if r.cache.GetState(2) != StateOnDisk || r.cache.GetState(3) != StateFailed {
		t.Fatal("probe altered unrelated healthy or failed segments")
	}
}

func TestRecoveryResetWaitsForPreviousFailedPromise(t *testing.T) {
	cache := newIntegrityTestCache(t, SegmentMeta{MessageID: "fixture", Bytes: 8, EndOffset: 7})
	cache.MarkFailed(0, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430})
	promise := &fetchPromise{done: make(chan struct{})}
	sf := &SegmentFetcher{cache: cache, ctx: t.Context(), inFlight: map[int]*fetchPromise{0: promise}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sf.resetFailedForRetry(ctx, 0); !errors.Is(err, context.Canceled) || cache.GetState(0) != StateFailed {
		t.Fatalf("reset reopened a slot whose previous promise was still present: %v", err)
	}
	delete(sf.inFlight, 0)
	close(promise.done)
	if err := sf.resetFailedForRetry(t.Context(), 0); err != nil || cache.GetState(0) != StateEmpty {
		t.Fatalf("retired failed slot was not reopened: %v", err)
	}
	if !cache.MarkFetching(0) {
		t.Fatal("slot not available for a new fetch")
	}
	if err := sf.resetFailedForRetry(t.Context(), 0); err != nil || cache.GetState(0) != StateFetching {
		t.Fatalf("reset clobbered an active fetch: %v", err)
	}
}

func TestRecoveryProbeTargetsLogicalByteWhenIVUnavailable(t *testing.T) {
	r, plain := newEOFTestReader(t, 64, 16, true)
	previousBlock, ok := r.cache.Get(1)
	if !ok {
		t.Fatal("missing synthetic IV block")
	}
	cause := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430}
	for _, i := range []int{0, 1} {
		r.cache.invalidateForRefetch(i)
		r.cache.MarkFailed(i, cause)
	}
	var fetched int
	r.fetcher.config.RecoveredArticle = func(_ context.Context, seg SegmentMeta, w io.Writer) (bool, error) {
		if seg.Number != 2 {
			return false, errors.New("probe requested an unrelated cipher block")
		}
		fetched++
		_, err := w.Write(previousBlock)
		return true, err
	}
	var one [1]byte
	_, err := r.ReadAt(one[:], 33)
	var failure *SegmentReadFailure
	if !errors.As(err, &failure) || failure.Offset != 33 {
		t.Fatalf("missing IV did not identify the dependent logical byte: %v", err)
	}
	n, err := r.ReadAtContext(WithRecoveryProbe(t.Context()), one[:], failure.Offset)
	if err != nil || n != 1 || one[0] != plain[33] || fetched != 1 || r.cache.GetState(0) != StateFailed {
		t.Fatalf("IV recovery read %d bytes, error=%v, fetched=%d", n, err, fetched)
	}
}
