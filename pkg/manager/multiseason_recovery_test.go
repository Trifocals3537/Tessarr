package manager

import (
	"os"
	"testing"
	"time"

	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
)

func TestRecoverCompletedSeasonFanouts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		actualOutput bool
		staleChild   bool
		wantHidden   bool
	}{
		{"legacy phantom parent", false, false, true},
		{"legitimate parent output", true, false, false},
		{"older child from prior queue entry", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, parent := newMultiSeasonReadinessManager(t)
			_, seasons := m.downloader.detectMultiSeason(parent)
			for _, child := range convertToMultiSeason(parent, seasons) {
				child.MarkAsCompleted(child.DownloadPath())
				child.Status = debridTypes.TorrentStatusDownloading // legacy completion
				if err := m.queue.Add(child); err != nil {
					t.Fatal(err)
				}
				if tc.staleChild {
					child.CreatedAt = parent.CreatedAt.Add(-time.Hour)
					if err := m.queue.Update(child); err != nil {
						t.Fatal(err)
					}
				}
			}
			parent.MarkAsCompleted(parent.DownloadPath())
			if err := m.queue.Update(parent); err != nil {
				t.Fatal(err)
			}
			if tc.actualOutput {
				if err := os.MkdirAll(parent.DownloadPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 { // idempotent on a subsequent startup
				if err := m.recoverCompletedSeasonFanouts(t.Context()); err != nil {
					t.Fatal(err)
				}
				current, err := m.queue.GetTorrent(parent.InfoHash)
				if err != nil || current.IsSeasonFanoutParent() != tc.wantHidden {
					t.Fatalf("recovered parent = %+v, error = %v, want hidden=%v", current, err, tc.wantHidden)
				}
				if tc.wantHidden && current.ContentPath != "" {
					t.Fatalf("phantom parent retained import path: %q", current.ContentPath)
				}
				if !tc.wantHidden && current.ContentPath != parent.DownloadPath() {
					t.Fatalf("unrelated parent path changed: %q", current.ContentPath)
				}
				for _, season := range seasons {
					child, err := m.queue.GetTorrent(season.InfoHash)
					if err != nil || child.Status != debridTypes.TorrentStatusDownloaded || !child.IsComplete {
						t.Fatalf("legacy child was not normalized: %+v %v", child, err)
					}
				}
			}
		})
	}
}
