package storage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

func TestQueuedOutcomeCrashHelper(t *testing.T) {
	root := os.Getenv("TESSARR_OUTCOME_CRASH_ROOT")
	if root == "" {
		return
	}
	s, err := NewStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := &Entry{InfoHash: "crashed-outcome", State: EntryStateDownloading}
	if err := s.AddQueue(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitQueuedOutcome(entry, QueueOutcome{Kind: QueueOutcomeCompleted, ContentPath: "owned-output"}); err != nil {
		t.Fatal(err)
	}
	os.Exit(86) // Deliberately bypass final Close/sync and OS-lock cleanup.
}

func TestSeasonFanoutCompletionDurability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "db")
	s, err := NewStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			_ = s.Close()
		}
	})
	parent := &Entry{InfoHash: "parent", Protocol: config.ProtocolTorrent, State: EntryStateDownloading}
	child := &Entry{
		InfoHash: "child", Protocol: config.ProtocolTorrent, State: EntryStateDownloading,
		Status: debridTypes.TorrentStatusDownloading, FileScopeRestricted: true,
		SeasonSource: &SeasonSource{InfoHash: parent.InfoHash, Season: 1}, SkipMultiSeason: true,
	}
	for _, entry := range []*Entry{parent, child} {
		if err := s.AddQueue(entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitQueuedOutcome(parent, QueueOutcome{
		Kind: QueueOutcomeCompleted, ContentPath: "uncreated", SeasonFanoutComplete: true,
	}); err == nil {
		t.Fatal("fanout completion accepted an independently claimed path")
	}
	if _, err := s.CommitQueuedOutcome(parent, QueueOutcome{
		Kind: QueueOutcomeCompleted, SeasonFanoutComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitQueuedOutcome(child, QueueOutcome{
		Kind: QueueOutcomeCompleted, ContentPath: "season-output",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetQueued(parent.InfoHash)
	if err != nil || !p.IsSeasonFanoutParent() || p.ContentPath != "" {
		t.Fatalf("fanout marker lost after restart: %+v %v", p, err)
	}
	c, err := s.GetQueued(child.InfoHash)
	if err != nil || !c.IsComplete || c.Status != debridTypes.TorrentStatusDownloaded || c.ContentPath != "season-output" || c.IsSeasonFanoutParent() {
		t.Fatalf("completed child lost import state after restart: %+v %v", c, err)
	}
}

func TestQueuedOutcomeReopensAfterCrashWithoutFinalSync(t *testing.T) {
	root := filepath.Join(t.TempDir(), "db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestQueuedOutcomeCrashHelper$")
	cmd.Env = append(os.Environ(), "TESSARR_OUTCOME_CRASH_ROOT="+root)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("crash helper: %v\n%s", err, output)
	}
	s, err := NewStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entry, err := s.GetQueued("crashed-outcome")
	if err != nil || !entry.IsComplete || entry.ContentPath != "owned-output" || entry.QueueIncarnation == "" {
		t.Fatalf("outcome not recovered after crash: %+v %v", entry, err)
	}
}

func TestQueuedOutcomePersistsFreshMetadataAndFencesReplacements(t *testing.T) {
	for _, kind := range []QueueOutcomeKind{QueueOutcomeFailed, QueueOutcomeDeferred, QueueOutcomeCompleted} {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			s, err := NewStorage(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			expected := &Entry{InfoHash: "outcome", Name: "release", State: EntryStateDownloading}
			if err := s.AddQueue(expected); err != nil {
				t.Fatal(err)
			}
			current, err := s.GetQueued(expected.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			current.Files = map[string]*File{"fresh.mkv": {Name: "fresh.mkv", Size: 128}}
			if err := s.UpdateQueue(current); err != nil {
				t.Fatal(err)
			}
			outcome := QueueOutcome{Kind: kind, Err: errors.New("operation failed"), ContentPath: "owned-output"}
			updated, err := s.CommitQueuedOutcome(expected, outcome)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Files["fresh.mkv"] == nil {
				t.Fatal("outcome overwrote newer file metadata")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = NewStorage(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			updated, err = s.GetQueued(expected.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case QueueOutcomeFailed:
				if updated.State != EntryStateError || updated.LastError != outcome.Err.Error() {
					t.Fatal(updated)
				}
			case QueueOutcomeDeferred:
				if updated.State != EntryStateDownloading || updated.Status != debridTypes.TorrentStatusQueued {
					t.Fatal(updated)
				}
			case QueueOutcomeCompleted:
				if !updated.IsComplete || updated.ContentPath != outcome.ContentPath {
					t.Fatal(updated)
				}
				if _, err := s.CommitQueuedOutcome(expected, QueueOutcome{Kind: QueueOutcomeFailed, Err: errors.New("late failure")}); !errors.Is(err, ErrQueueOutcomeConflict) {
					t.Fatalf("late failure overwrote success: %v", err)
				}
			}
			if err := s.DeleteQueued(expected.InfoHash, nil); err != nil {
				t.Fatal(err)
			}
			replacement := &Entry{InfoHash: expected.InfoHash, Name: "replacement", State: EntryStateDownloading}
			if err := s.AddQueue(replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitQueuedOutcome(expected, outcome); !errors.Is(err, ErrQueueOutcomeConflict) {
				t.Fatalf("replacement accepted old outcome: %v", err)
			}
			if _, err := s.PrepareQBitQueuedDeletion(replacement.InfoHash, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitQueuedOutcome(replacement, outcome); !errors.Is(err, ErrQueuedEntryDeleting) {
				t.Fatalf("deletion accepted an outcome: %v", err)
			}
		})
	}
}

func TestStorageOwnerExcludesRecoveryAndLegacyCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	first, err := NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := NewStorage(path); err == nil {
		_ = second.Close()
		t.Fatal("second storage writer accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewStorage(path)
	if err != nil {
		t.Fatal("closed storage retained ownership", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
