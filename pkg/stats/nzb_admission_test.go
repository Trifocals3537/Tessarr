package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/logger"
	"github.com/Trifocals3537/tessarr/pkg/manager"
)

func TestStatsSnapshotIncludesBoundedNZBRejections(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	config.Get().DownloadFolder = t.TempDir()
	t.Cleanup(config.Reset)
	mgr := manager.New()
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Error(err)
		}
		if err := logger.Close(); err != nil {
			t.Error(err)
		}
	})
	collector := New(mgr)
	_, err := collector.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	collector.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/debug/stats", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("stats status = %d", response.Code)
	}
	data := response.Body.Bytes()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	admission, ok := fields["nzb_admission"]
	if !ok {
		t.Fatal("stats omit NZB rejection diagnostics for attempts that never enter the queue")
	}
	var summary struct {
		TotalRejected uint64 `json:"total_rejected"`
		ByReason      []struct {
			Reason         string `json:"reason"`
			Count          uint64 `json:"count"`
			LastRejectedAt string `json:"last_rejected_at"`
		} `json:"by_reason"`
	}
	if err := json.Unmarshal(admission, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.TotalRejected != 0 || len(summary.ByReason) != 8 {
		t.Fatalf("initial summary = %+v, want zero rejections and eight fixed reasons", summary)
	}
	seen := make(map[string]bool)
	allowed := map[string]bool{
		"articles_unavailable": true, "invalid_nzb": true, "access_denied": true,
		"timeout": true, "provider_unavailable": true, "cancelled": true,
		"mixed": true, "other_parse_error": true,
	}
	for _, reason := range summary.ByReason {
		if !allowed[reason.Reason] || seen[reason.Reason] || reason.Count != 0 || reason.LastRejectedAt != "" {
			t.Fatalf("invalid initial reason: %+v", reason)
		}
		seen[reason.Reason] = true
	}
}
