package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/arr"
	"github.com/Trifocals3537/tessarr/pkg/debrid/providers/torbox"
	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/rs/zerolog"
)

func TestTorBoxCheckingPollUpdatesWithoutHandoff(t *testing.T) {
	for _, state := range []string{"checking", " CHECKING (Verifying data) ", "checkingResumeData", "checkingUP", "checkingDL"} {
		t.Run(state, func(t *testing.T) {
			m := newQueueRecoveryTestManager(t)
			m.uncachedStallTimeout = 30 * time.Minute
			var logs bytes.Buffer
			m.logger = zerolog.New(&logs)
			var polls, mutations, arrRequests, progress atomic.Int32
			providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/torrents/mylist" || r.URL.Query().Get("id") != "42" {
					mutations.Add(1)
					t.Errorf("unexpected provider operation: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected operation", http.StatusBadRequest)
					return
				}
				if r.URL.Query().Get("bypass_cache") == "true" {
					t.Error("checking unexpectedly triggered fresh failure confirmation")
				}
				polls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
					"id": 42, "name": "release.mkv", "download_state": state,
					"download_finished": false, "progress": float64(progress.Load()) / 100,
					"size": 4096, "seeds": 7, "download_speed": 0, "files": []any{},
				}})
			}))
			defer providerServer.Close()
			arrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				arrRequests.Add(1)
				http.Error(w, "checking must not trigger Arr handoff", http.StatusBadRequest)
			}))
			defer arrServer.Close()
			provider, err := torbox.New(config.Debrid{Name: "torbox", Provider: "torbox", APIKey: "fixture-key"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			provider.Host = providerServer.URL
			m.clients.Store("torbox", provider)
			m.arr.AddOrUpdate(arr.New("sonarr", arrServer.URL, "fixture-token", false, nil, "torbox", "download"))

			entry := interruptedQueueTestEntry("0123456789abcdef0123456789abcdef01234567")
			entry.Status = debridTypes.TorrentStatusDownloading
			entry.IsDownloading = false
			entry.Progress = 0.25
			entry.TerminalChecks = 1
			entry.ClientEndpoint = "http://tessarr.example:8282"
			entry.Providers["torbox"].ID = "42"
			// Queue timestamps are persisted at one-second precision.
			stale := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
			entry.LastProgressAt = &stale
			entry.LastObservedAt = &stale
			if err := m.queue.Add(entry); err != nil {
				t.Fatal(err)
			}
			entry, err = m.queue.GetTorrent(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			incarnation := entry.QueueIncarnation
			key := uncachedStallCandidateKey(entry)
			m.stallCandidates = map[string]stallCandidate{
				key: {kind: "stalled", checkedAt: time.Now().Add(-time.Minute)},
			}

			for i, percent := range []int32{25, 25, 35, 100} {
				progress.Store(percent)
				pollStarted := time.Now().Truncate(time.Second)
				if err := m.processQueuedTorrent(context.Background(), entry); err != nil {
					t.Fatal(err)
				}
				entry, err = m.queue.GetTorrent(entry.InfoHash)
				if err != nil {
					t.Fatal(err)
				}
				if entry.State != storage.EntryStateDownloading || entry.Status != debridTypes.TorrentStatusDownloading || entry.HandoffReason != "" || entry.TerminalChecks != 0 {
					t.Fatalf("checking became a failure or completion: state=%s status=%s handoff=%q terminalChecks=%d", entry.State, entry.Status, entry.HandoffReason, entry.TerminalChecks)
				}
				if entry.LastObservedAt == nil || entry.LastObservedAt.Before(pollStarted) {
					t.Fatal("checking did not persist a successful observation")
				}
				if entry.LastProgressAt == nil || (i < 2 && !entry.LastProgressAt.Equal(stale)) || (i >= 2 && !entry.LastProgressAt.After(stale)) {
					t.Fatalf("checking confused polling with forward progress: %v", entry.LastProgressAt)
				}
				if entry.Progress != float64(percent)/100 || entry.Size != 4096 || entry.Seeders != 7 || entry.Speed != 0 {
					t.Fatalf("checking metadata was not updated: progress=%v size=%d seeds=%d speed=%d", entry.Progress, entry.Size, entry.Seeders, entry.Speed)
				}
				placement := entry.GetActiveProvider()
				if entry.QueueIncarnation != incarnation || placement == nil || placement.ID != "42" || placement.Progress != entry.Progress {
					t.Fatal("checking changed the placement identity or lost its progress")
				}
				m.stallCandidatesMu.Lock()
				_, pending := m.stallCandidates[key]
				m.stallCandidatesMu.Unlock()
				if pending {
					t.Fatal("checking retained pending no-progress failure evidence")
				}
				if _, err := m.storage.Get(entry.InfoHash); !storage.IsEntryNotFound(err) {
					t.Fatalf("checking published a main entry: %v", err)
				}
				if err := m.handoffUncachedFailures(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if polls.Load() != 4 || mutations.Load() != 0 || arrRequests.Load() != 0 {
				t.Fatalf("checking caused extra or destructive requests: polls=%d provider mutations=%d Arr requests=%d", polls.Load(), mutations.Load(), arrRequests.Load())
			}
			if m.uncachedFreshChecks.Load() != 0 || m.uncachedStallConfirmed.Load() != 0 || m.uncachedTerminalConfirmed.Load() != 0 || m.uncachedHandoffAccepted.Load() != 0 {
				t.Fatal("checking triggered failure confirmation or handoff")
			}
			if strings.Contains(logs.String(), "unclassified uncached error") {
				t.Fatal("checking still emits the unclassified-error warning")
			}
		})
	}
}
