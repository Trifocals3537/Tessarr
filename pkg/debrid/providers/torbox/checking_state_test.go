package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/customerror"
	"github.com/Trifocals3537/tessarr/internal/request"
	"github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/rs/zerolog"
)

func TestCheckStatusClassifiesCheckingWithoutCompletion(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	for _, fresh := range []bool{false, true} {
		for _, test := range []struct {
			name, state       string
			finished, cached  bool
			status            types.TorrentStatus
			wantErr, terminal bool
		}{
			{name: "bare checking", state: "checking", status: types.TorrentStatusDownloading},
			{name: "case and whitespace", state: " CHECKING ", status: types.TorrentStatusDownloading},
			{name: "details", state: "checking (Verifying data)", status: types.TorrentStatusDownloading},
			{name: "resume variant", state: "checkingResumeData", status: types.TorrentStatusDownloading},
			{name: "upload variant", state: "checkingUP", status: types.TorrentStatusDownloading},
			{name: "download variant", state: "checkingDL", status: types.TorrentStatusDownloading},
			{name: "cached only", state: "checking", cached: true, status: types.TorrentStatusDownloading, wantErr: true},
			{name: "authoritative completion", state: "checking", finished: true, status: types.TorrentStatusDownloaded},
			{name: "unknown", state: "future-provider-state", status: types.TorrentStatusError, wantErr: true},
			{name: "unknown checking suffix", state: "checkingFuture", status: types.TorrentStatusError, wantErr: true},
			{name: "terminal", state: "failed (processing)", status: types.TorrentStatusError, wantErr: true, terminal: true},
		} {
			name := "ordinary/"
			if fresh {
				name = "fresh/"
			}
			t.Run(name+test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/torrents/mylist" || r.URL.Query().Get("id") != "42" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					if (r.URL.Query().Get("bypass_cache") == "true") != fresh {
						t.Errorf("bypass_cache = %q, fresh = %t", r.URL.Query().Get("bypass_cache"), fresh)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
						"id": 42, "name": "release.mkv", "download_state": test.state,
						"download_finished": test.finished, "progress": 1, "files": []any{},
					}})
				}))
				defer server.Close()
				client := &Torbox{
					Host: server.URL, config: config.Debrid{Name: "torbox"}, logger: zerolog.Nop(),
					client: request.New(request.WithMaxRetries(0), request.WithTimeout(2*time.Second)),
				}
				torrent := &types.Torrent{Id: "42", DownloadUncached: !test.cached}
				check := client.CheckStatusContext
				if fresh {
					check = client.CheckStatusFreshContext
				}
				got, err := check(context.Background(), torrent)
				if (err != nil) != test.wantErr || errors.Is(err, types.ErrTerminalProviderTorrent) != test.terminal {
					t.Fatalf("error = %v, want error=%t terminal=%t", err, test.wantErr, test.terminal)
				}
				if got == nil || got.Id != "42" || got.ProviderState != test.state || got.Status != test.status || got.Progress != 100 || len(got.Files) != 0 {
					t.Fatalf("status response = %+v, want state %q/status %q with no invented files", got, test.state, test.status)
				}
				if test.cached {
					var cacheMiss *customerror.Error
					if !errors.As(err, &cacheMiss) || cacheMiss.Code != "torrent_not_cached" || !cacheMiss.IsRetryable() {
						t.Fatalf("cached-only checking lost its not-cached classification: %v", err)
					}
				}
			})
		}
	}
}

func TestCheckingStatusDoesNotMaskAPIFailure(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"success":false}`))
		}))
		client := &Torbox{
			Host: server.URL, logger: zerolog.Nop(), config: config.Debrid{Name: "torbox"},
			client: request.New(request.WithMaxRetries(0), request.WithTimeout(2*time.Second)),
		}
		torrent, err := client.CheckStatusContext(context.Background(), &types.Torrent{
			Id: "42", DownloadUncached: true, ProviderState: "checking", Status: types.TorrentStatusDownloading,
		})
		server.Close()
		if err == nil || errors.Is(err, types.ErrTerminalProviderTorrent) || torrent.Status == types.TorrentStatusDownloaded {
			t.Fatalf("HTTP %d masked as completion or terminal evidence: %+v, %v", status, torrent, err)
		}
	}
}
