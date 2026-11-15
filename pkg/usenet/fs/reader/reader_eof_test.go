package reader

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	appConfig "github.com/Trifocals3537/tessarr/internal/config"
	appCrypto "github.com/Trifocals3537/tessarr/internal/crypto"
	"github.com/Trifocals3537/tessarr/internal/nntp"
)

// Cache-focused tests use the real reader/cache/fetcher with preloaded public
// bytes and no provider. Parser integration tests separately exercise NNTP.
func newEOFTestReader(t *testing.T, size, chunk int, encrypted bool) (*StreamingReader, []byte) {
	t.Helper()
	appConfig.SetConfigPath(t.TempDir())
	plain := make([]byte, size)
	for i := range plain {
		plain[i] = byte(i*31 + i/17 + 7)
	}
	stored := append([]byte(nil), plain...)
	key, iv := bytes.Repeat([]byte{0x42}, 32), bytes.Repeat([]byte{0x23}, aes.BlockSize)
	if encrypted {
		if size%aes.BlockSize != 0 {
			t.Fatal("encrypted fixture must contain complete physical AES blocks")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(stored, stored)
	}
	var segments []SegmentMeta
	for off := 0; off < size; off += chunk {
		end := min(off+chunk, size)
		segments = append(segments, SegmentMeta{MessageID: fmt.Sprintf("cached-%d@fixture.invalid", off), Number: len(segments) + 1, Bytes: int64(end - off), StartOffset: int64(off), EndOffset: int64(end - 1)})
	}
	reader, err := NewStreamingReaderWithEncryption(t.Context(), &nntp.Client{}, segments,
		EncryptionConfig{Enabled: encrypted, Key: key, IV: iv}, WithDiskPath(t.TempDir()),
		WithPrefetchAhead(0), WithMaxConnections(1), func(c *Config) { c.MaxRetries = 1 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	for index, segment := range segments {
		if err := reader.cache.Put(index, stored[segment.StartOffset:segment.EndOffset+1]); err != nil {
			t.Fatal(err)
		}
	}
	return reader, plain
}

func checkEOFRead(read func([]byte, int64) (int, error), plain []byte, off int64, length int) error {
	got := bytes.Repeat([]byte{0xcc}, length)
	want := bytes.Repeat([]byte{0xcc}, length)
	wantN, wantErr := 0, error(nil)
	// Empty reads retain StreamingReader's existing no-op behavior, including
	// at/past EOF. Nonempty calls are checked against the standard ReaderAt.
	if length > 0 {
		wantN, wantErr = bytes.NewReader(plain).ReadAt(want, off)
	}
	n, err := read(got, off)
	if n != wantN || !errors.Is(err, wantErr) || !bytes.Equal(got, want) {
		return fmt.Errorf("ReadAt(off=%d,len=%d)=(%d,%v,%x), want (%d,%v,%x)", off, length, n, err, got, wantN, wantErr, want)
	}
	return nil
}

func assertEOFRead(t *testing.T, read func([]byte, int64) (int, error), plain []byte, off int64, length int) {
	t.Helper()
	if err := checkEOFRead(read, plain, off, length); err != nil {
		t.Fatal(err)
	}
}

func TestStreamingReaderAtEOFBoundaries(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		sizes := []int{1, 15, 16, 17, 31, 32, 33, 64, 65}
		if encrypted {
			sizes = []int{16, 32, 64, 80}
		}
		for _, size := range sizes {
			for _, chunk := range []int{13, 17} {
				t.Run(fmt.Sprintf("encrypted=%v/size=%d/chunk=%d", encrypted, size, chunk), func(t *testing.T) {
					reader, plain := newEOFTestReader(t, size, chunk, encrypted)
					readWithNilContext := func(p []byte, off int64) (int, error) {
						//lint:ignore SA1012 Intentionally exercise this API's existing nil-context compatibility.
						return reader.ReadAtContext(nil, p, off)
					}
					positions := []int64{0, int64(size / 2), int64(size - 1), int64(size), int64(size + 1), math.MaxInt64}
					for _, boundary := range []int64{1, 15, 16, 17} {
						if boundary < int64(size) {
							positions = append(positions, boundary)
						}
					}
					for _, off := range positions {
						for _, length := range []int{0, 1, 2, 15, 16, 17, size, size + 1} {
							assertEOFRead(t, reader.ReadAt, plain, off, length)
							assertEOFRead(t, readWithNilContext, plain, off, length)
						}
					}
				})
			}
		}
	}
}

func TestStreamingReaderAtEOFPreservesErrors(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, kind := range []string{"canceled", "deadline", "fetch", "closed"} {
			t.Run(fmt.Sprintf("encrypted=%v/%s", encrypted, kind), func(t *testing.T) {
				reader, plain := newEOFTestReader(t, 64, 16, encrypted)
				ctx := context.Background()
				var want error
				switch kind {
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					want = context.Canceled
				case "deadline":
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
					want = context.DeadlineExceeded
				case "fetch":
					want = errors.New("synthetic segment I/O failure")
					reader.cache.MarkFailed(reader.segCount-1, want)
				case "closed":
					if err := reader.Close(); err != nil {
						t.Fatal(err)
					}
					want = io.ErrClosedPipe
				}
				got := bytes.Repeat([]byte{0xcc}, 2)
				n, err := reader.ReadAtContext(ctx, got, int64(len(plain)-1))
				if n != 0 || !errors.Is(err, want) || errors.Is(err, io.EOF) || !bytes.Equal(got, []byte{0xcc, 0xcc}) {
					t.Fatalf("error became EOF/data: n=%d err=%v want=%v data=%x", n, err, want, got)
				}
			})
		}
	}
	t.Run("decryption", func(t *testing.T) {
		reader, _ := newEOFTestReader(t, 16, 16, true)
		reader.encryption.Key = []byte{1}
		if n, err := reader.ReadAt(make([]byte, 2), 15); n != 0 || !errors.Is(err, appCrypto.ErrInvalidKeySize) || errors.Is(err, io.EOF) {
			t.Fatalf("decryption error became EOF: n=%d err=%v", n, err)
		}
	})
	t.Run("previous CBC block", func(t *testing.T) {
		reader, _ := newEOFTestReader(t, 64, 16, true)
		want := errors.New("synthetic IV-block I/O failure")
		reader.cache.MarkFailed(2, want) // Tail ciphertext is readable; its IV is not.
		if n, err := reader.ReadAt(make([]byte, 2), 63); n != 0 || !errors.Is(err, want) || errors.Is(err, io.EOF) {
			t.Fatalf("IV error became EOF: n=%d err=%v", n, err)
		}
	})
}

type eofWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *eofWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestStreamingReaderAtEOFCancelBlockedRead(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			reader, plain := newEOFTestReader(t, 64, 16, encrypted)
			last := reader.segCount - 1
			promise := &fetchPromise{done: make(chan struct{})}
			reader.cache.SetState(last, StateFetching)
			reader.fetcher.inFlight[last] = promise
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &eofWaitContext{Context: base, waiting: make(chan struct{})}
			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			got := bytes.Repeat([]byte{0xcc}, 2)
			go func() { n, err := reader.ReadAtContext(ctx, got, 63); done <- result{n, err} }()
			select {
			case <-ctx.waiting:
			case <-time.After(3 * time.Second):
				t.Fatal("reader never waited for segment")
			}
			cancel()
			select {
			case outcome := <-done:
				if outcome.n != 0 || !errors.Is(outcome.err, context.Canceled) || !bytes.Equal(got, []byte{0xcc, 0xcc}) {
					t.Fatalf("blocked cancellation: %+v bytes=%x", outcome, got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("canceled read did not return")
			}
			reader.fetcher.inFlightMu.Lock()
			delete(reader.fetcher.inFlight, last)
			reader.fetcher.inFlightMu.Unlock()
			close(promise.done)
			reader.cache.SetState(last, StateOnDisk)
			assertEOFRead(t, reader.ReadAt, plain, 63, 2)
		})
	}
}

func TestStreamingReaderAtEOFConsumers(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			reader, plain := newEOFTestReader(t, 64, 13, encrypted)
			oversized := make([]byte, len(plain)+1)
			if n, err := io.ReadFull(reader, oversized); n != len(plain) || !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(oversized[:n], plain) {
				t.Fatalf("ReadFull: n=%d err=%v", n, err)
			}
			if off, err := reader.Seek(-1, io.SeekEnd); off != 63 || err != nil {
				t.Fatal(off, err)
			}
			if n, err := reader.Read(make([]byte, 2)); n != 1 || !errors.Is(err, io.EOF) {
				t.Fatalf("sequential tail: n=%d err=%v", n, err)
			}
			if off, err := reader.Seek(0, io.SeekCurrent); off != 64 || err != nil {
				t.Fatal(off, err)
			}
			if _, err := reader.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, plain) {
				t.Fatal("ReadAll mismatch", err)
			}
			section := io.NewSectionReader(reader, 0, 17)
			buf := bytes.Repeat([]byte{0xcc}, 18)
			if n, err := section.ReadAt(buf, 0); n != 17 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:17], plain[:17]) || buf[17] != 0xcc {
				t.Fatalf("bounded logical member: n=%d err=%v", n, err)
			}
		})
	}
}

func TestStreamingReaderAtEOFConcurrentReads(t *testing.T) {
	reader, plain := newEOFTestReader(t, 80, 13, true)
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for worker := range 8 {
		workers.Go(func() {
			for iteration := range 50 {
				off := int64((iteration*17 + worker*13) % (len(plain) + 1))
				if err := checkEOFRead(reader.ReadAt, plain, off, 1+iteration%33); err != nil {
					failures <- err
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
