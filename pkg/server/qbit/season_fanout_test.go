package qbit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/logger"
	"github.com/Trifocals3537/tessarr/pkg/manager"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

func TestQBitSeasonFanoutOnlyExposesImportableEntries(t *testing.T) {
	previousPath := config.GetMainPath()
	if err := config.SetConfigPath(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.SetConfigPath(previousPath) })
	m := manager.New()
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Error(err)
		}
		if err := logger.Close(); err != nil {
			t.Error(err)
		}
	})
	parent := &storage.Entry{
		InfoHash: "parent", Protocol: config.ProtocolTorrent, Name: "series S01", Category: "tv",
		State: storage.EntryStatePausedUP, IsComplete: true, Progress: 1, SeasonFanoutComplete: true,
	}
	child := &storage.Entry{
		InfoHash: "child", Protocol: config.ProtocolTorrent, Name: "series S01", Category: "tv",
		State: storage.EntryStatePausedUP, IsComplete: true, Progress: 1, ContentPath: "season-output",
		FileScopeRestricted: true, SeasonSource: &storage.SeasonSource{InfoHash: parent.InfoHash, Season: 1},
	}
	ordinary := &storage.Entry{
		InfoHash: "ordinary", Protocol: config.ProtocolTorrent, Name: "other", Category: "tv",
		State: storage.EntryStatePausedUP, IsComplete: true, Progress: 1, ContentPath: "ordinary-output",
	}
	for _, entry := range []*storage.Entry{parent, child, ordinary} {
		if err := m.Queue().Add(entry); err != nil {
			t.Fatal(err)
		}
	}
	q := &QBit{manager: m}
	for _, hashes := range [][]string{nil, {"parent", "child", "ordinary"}} {
		req := httptest.NewRequest(http.MethodGet, "/torrents/info", nil)
		ctx := context.WithValue(req.Context(), categoryKey, "tv")
		ctx = context.WithValue(ctx, hashesKey, hashes)
		res := httptest.NewRecorder()
		q.handleTorrentsInfo(res, req.WithContext(ctx))
		var entries []Torrent
		if err := json.Unmarshal(res.Body.Bytes(), &entries); err != nil || res.Code != http.StatusOK || len(entries) != 2 {
			t.Fatalf("qBittorrent listing = %s (%d), error = %v", res.Body.String(), res.Code, err)
		}
		for _, entry := range entries {
			if entry.Hash == parent.InfoHash || entry.ContentPath == "" {
				t.Fatalf("qBittorrent exposed a non-importable fanout parent: %+v", entry)
			}
		}
	}
	if _, err := m.Queue().GetTorrent(parent.InfoHash); err != nil {
		t.Fatalf("qBittorrent listing removed internal source tracking: %v", err)
	}
	for _, handler := range []http.HandlerFunc{q.handleTorrentProperties, q.handleTorrentFiles} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/torrents/files?hash=parent", nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("direct lookup exposed hidden parent: %d", res.Code)
		}
	}
}
