package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testGeneration = strings.Repeat("a", 64)
var testSet = strings.Repeat("b", 32)
var testMD5 = strings.Repeat("c", 32)
var testIdentity = Identity{NZBID: "synthetic-nzb", File: "example.mkv"}

func TestStoreOverlayMergesRangesAndPreservesIdentity(t *testing.T) {
	s := newTestStore(t)
	if err := s.Publish(t.Context(), testGeneration, 10, testSet, testMD5, []Block{{Offset: 2, Data: []byte("234")}}, nil, testIdentity); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(t.Context(), testGeneration, 10, testSet, testMD5, []Block{{Offset: 7, Data: []byte("78")}}, nil, testIdentity); err != nil {
		t.Fatal(err)
	}
	base := func(ctx context.Context, p []byte, off int64) (int, error) {
		for i := range p {
			if pos := off + int64(i); pos >= 2 && pos < 5 || pos >= 7 && pos < 9 {
				t.Fatal("base reader fetched repaired bytes")
			}
		}
		return bytes.NewReader([]byte("0123456789")).ReadAt(p, off)
	}
	for _, r := range [][2]int{{0, 10}, {1, 7}, {2, 3}, {7, 2}, {9, 2}} {
		p := make([]byte, r[1])
		n, err := s.Overlay(t.Context(), testGeneration, 10, p, int64(r[0]), base)
		want := []byte("0123456789")[r[0]:min(r[0]+r[1], 10)]
		if n != len(want) || !bytes.Equal(p[:n], want) {
			t.Fatalf("range %v differs: %q %v", r, p, err)
		}
		if r[0]+r[1] > 10 && !errors.Is(err, io.EOF) {
			t.Fatal("missing EOF")
		}
	}
	if !s.Covers(testGeneration, 2, 3) || s.Covers(testGeneration, 2, 4) {
		t.Fatal("incorrect coverage")
	}
	if err := s.Publish(t.Context(), testGeneration, 10, strings.Repeat("d", 32), testMD5, []Block{{Offset: 5, Data: []byte("5")}}, nil, testIdentity); err == nil {
		t.Fatal("accepted mismatched recovery set")
	}
}

func TestStoreCrashBeforePointerLeavesNoPublishedData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recovery")
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	s.beforePointer = func() error { return errors.New("synthetic process exit before pointer") }
	if err := s.Publish(t.Context(), testGeneration, 10, testSet, testMD5, []Block{{Offset: 2, Data: []byte("234")}}, nil, testIdentity); err == nil {
		t.Fatal("missing injected failure")
	}
	if s.Revision(testGeneration) != "" {
		t.Fatal("partial publication")
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated.fixture"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".patch") || strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatal("orphan not reclaimed")
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "unrelated.fixture")); err != nil || string(b) != "keep" {
		t.Fatal("cleanup touched unrelated file")
	}
}

func TestStoreRejectsCorruptObjectsAndResourceOverflow(t *testing.T) {
	for _, kind := range []string{"corruption", "overlap", "store limit", "stale fence", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "recovery")
			s, err := OpenStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			blocks := []Block{{Offset: 2, Data: []byte("234")}}
			ctx := t.Context()
			var validate func() error
			switch kind {
			case "overlap":
				blocks = append(blocks, Block{Offset: 3, Data: []byte("x")})
			case "store limit":
				f, e := os.Create(filepath.Join(root, "capacity-fixture.patch"))
				if e != nil {
					t.Fatal(e)
				}
				e = f.Truncate(MaxStoreBytes)
				_ = f.Close()
				if e != nil {
					t.Fatal(e)
				}
			case "stale fence":
				validate = func() error { return ErrStale }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err = s.Publish(ctx, testGeneration, 10, testSet, testMD5, blocks, validate, testIdentity)
			if kind != "corruption" {
				if err == nil {
					t.Fatal("accepted invalid publication")
				}
				if s.Revision(testGeneration) != "" {
					t.Fatal("published invalid data")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			object := s.Revision(testGeneration)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(root, object), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := f.Stat()
			if _, err := f.WriteAt([]byte{0xff}, info.Size()-1); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			s, err = OpenStore(root)
			if err != nil {
				t.Fatal(err)
			}
			baseCalled := false
			_, err = s.Overlay(t.Context(), testGeneration, 10, make([]byte, 3), 2, func(context.Context, []byte, int64) (int, error) { baseCalled = true; return 0, nil })
			if err == nil || baseCalled {
				t.Fatal("corrupt patch was served or silently bypassed")
			}
		})
	}
}
