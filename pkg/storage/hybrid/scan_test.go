package hybrid

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestScanPageBoundsReadsAndResumesStableSnapshot(t *testing.T) {
	store, err := New(Config{DataPath: filepath.Join(t.TempDir(), "scan.db"), SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	put := func(key string) {
		t.Helper()
		if err := store.Put(key, []byte(key), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		put(key)
	}

	var cursor ScanCursor
	var got []string
	scan := func(limit int, more func() bool, want []string, wantPending bool) {
		t.Helper()
		got = nil
		before := store.GetStats().Reads
		pending, err := store.ScanPage(&cursor, limit, more, func(key string, value []byte) error {
			got = append(got, key)
			if string(value) != key {
				t.Fatalf("value for %q = %q", key, value)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) || store.GetStats().Reads-before != int64(len(want)) || pending != wantPending {
			t.Fatalf("page reads/pending = %v/%d/%t, want %v/%d/%t", got, store.GetStats().Reads-before, pending, want, len(want), wantPending)
		}
	}
	scan(5, func() bool { return false }, nil, false)
	scan(5, func() bool { return len(got) == 0 }, []string{"a"}, true)
	// Disk order changes without invalidating the resumable snapshot.
	put("a")
	if err := store.Delete("c"); err != nil {
		t.Fatal(err)
	}
	put("g")
	scan(2, nil, []string{"b"}, true) // deleted c still consumes a visit
	scan(2, nil, []string{"d", "e"}, true)
	scan(2, nil, []string{"f"}, false)
	scan(10, nil, []string{"b", "d", "e", "f", "a", "g"}, false)
}
