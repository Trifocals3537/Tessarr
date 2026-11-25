package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/customerror"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/manager/link"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/rs/zerolog"
)

func TestClassifyTerminalReadFailure(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     string
		retryable bool
	}{
		{name: "canceled", err: context.Canceled, class: "canceled"},
		{name: "deadline", err: context.DeadlineExceeded, class: "timeout", retryable: true},
		{name: "missing article", err: &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430}, class: "content_missing"},
		{name: "invalid article", err: &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, ArticleReason: nntp.ArticleInvalidGeometry}, class: "invalid_article_data"},
		{name: "short article", err: &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, ArticleReason: nntp.ArticleIncompleteBody}, class: "invalid_article_data"},
		{name: "unclassified article", err: &nntp.Error{Type: nntp.ErrorTypeArticleNotFound}, class: "article_unavailable"},
		{name: "mixed providers", err: &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, ArticleReason: nntp.ArticleMixedUnavailable}, class: "article_unavailable"},
		{name: "deferred article", err: customerror.NewDeferredArticleNotFoundError(&nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430}), class: "local_cooldown", retryable: true},
		{name: "invalid map", err: fmt.Errorf("preflight: %w", storage.ErrInvalidNZBMap), class: "invalid_nzb_map"},
		{name: "authentication", err: &nntp.Error{Type: nntp.ErrorTypeAuthentication}, class: "provider_auth"},
		{name: "truncated", err: io.ErrUnexpectedEOF, class: "unexpected_eof", retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, retryable := classifyTerminalReadFailure(tt.err)
			if class != tt.class || retryable != tt.retryable {
				t.Fatalf("classification = %q/%v, want %q/%v", class, retryable, tt.class, tt.retryable)
			}
		})
	}
}

func TestUsenetDeferredFailurePreservesDirtyRevisionAndRedactsCause(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry := &storage.Entry{Name: "fixture-release", InfoHash: "fixture-hash", Protocol: config.ProtocolNZB}
	var logs bytes.Buffer
	m := &Manager{storage: store, logger: zerolog.New(&logs)}
	secret := "sensitive-article-id"
	missing := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: secret}
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 0, 1, missing)
	before, err := store.GetEntryHealth(entry.Name)
	if err != nil {
		t.Fatal(err)
	}
	before.ProbeProgress = &storage.HealthProbeProgress{Revision: before.DirtyRevision, StartedAt: time.Now(), Verified: map[string]string{"other.mkv": "proof"}}
	if err := store.SaveEntryHealth(before); err != nil {
		t.Fatal(err)
	}
	deferred := fmt.Errorf("stream: %w", customerror.NewDeferredArticleNotFoundError(missing))
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 5, 1, deferred)
	after, err := store.GetEntryHealth(entry.Name)
	if err != nil || after.DirtyRevision != before.DirtyRevision || len(after.FailedReads) != len(before.FailedReads) ||
		after.ProbeProgress == nil || after.ProbeProgress.Verified["other.mkv"] != "proof" {
		t.Fatalf("deferred article created new evidence: %+v, %v", after, err)
	}
	if isDefinitiveRangeFailure(deferred) {
		t.Fatal("deferred result was treated as a definitive range failure")
	}
	diagnostic := DiagnoseStreamFailure(deferred)
	if diagnostic.Class != "local_cooldown" || diagnostic.Status != 410 || diagnostic.ArticleReason != "" {
		t.Fatalf("deferred diagnostic = %+v", diagnostic)
	}
	if s := logs.String(); strings.Contains(s, secret) || !strings.Contains(s, `"upstream_failure_observed":false`) {
		t.Fatalf("deferral diagnostics incorrect: %s", s)
	}
	logs.Reset()
	badData := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, ArticleReason: nntp.ArticleIncompleteBody, Message: secret}
	joined := fmt.Errorf("combined: %w", errors.Join(deferred, badData))
	if class, _ := classifyTerminalReadFailure(joined); class != "invalid_article_data" || readArticleFailureReason(joined) != string(nntp.ArticleIncompleteBody) {
		t.Fatal("cached branch obscured newly observed invalid article")
	}
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 5, 1, badData)
	after, err = store.GetEntryHealth(entry.Name)
	if err != nil || after.DirtyRevision <= before.DirtyRevision || after.DirtyReason != "read_failure:invalid_article_data" {
		t.Fatalf("fresh invalid-data evidence not persisted: %+v, %v", after, err)
	}
	if s := logs.String(); strings.Contains(s, secret) || !strings.Contains(s, `"article_failure_reason":"incomplete_decoded_body"`) || !strings.Contains(s, `"upstream_failure_observed":true`) {
		t.Fatalf("fresh invalid article diagnostics incorrect: %s", s)
	}
	logs.Reset()
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 0, 32, storage.ErrInvalidNZBMap)
	after, err = store.GetEntryHealth(entry.Name)
	if err != nil || after.DirtyReason != "read_failure:invalid_nzb_map" || !strings.Contains(logs.String(), `"upstream_failure_observed":false`) {
		t.Fatalf("invalid local map was not distinct from an upstream failure: %+v, %v, %s", after, err, logs.String())
	}
}

func TestRecordTerminalReadFailureMarksDirtyRateLimitsAndRedactsError(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var logs bytes.Buffer
	m := &Manager{storage: store, logger: zerolog.New(&logs)}
	m.repair = NewRepair(m)
	m.repair.eventMu.Lock()
	m.repair.eventStarted = true
	m.repair.eventMu.Unlock()
	entry := &storage.Entry{
		Name:           "Movie.Release",
		InfoHash:       "safe-hash",
		Protocol:       config.ProtocolNZB,
		ActiveProvider: "",
	}
	secret := "signed-token-must-not-appear"
	readErr := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: secret}

	m.RecordTerminalReadFailure(entry, "Movie.mkv", 1024, 4096, readErr)
	m.RecordTerminalReadFailure(entry, "Movie.mkv", 1024, 4096, readErr)

	state, err := store.GetEntryHealth(entry.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Dirty || state.DirtyReason != "read_failure:content_missing" || state.Protocol != config.ProtocolNZB {
		t.Fatalf("health state = %+v", state)
	}
	if got := m.terminalReadFailures.Load(); got != 2 {
		t.Fatalf("terminal read failures = %d, want 2", got)
	}
	if status := m.repair.eventQueueStatus(); status.Pending != 1 || status.Coalesced != 1 {
		t.Fatalf("event queue status = %+v, want one pending and one coalesced dirty event", status)
	}
	output := logs.String()
	if count := strings.Count(output, "Terminal media read failure"); count != 1 {
		t.Fatalf("terminal read failure log count = %d, want 1", count)
	}
	if strings.Contains(output, secret) {
		t.Fatal("raw provider error leaked into structured log")
	}
	if got := strings.Count(output, "Terminal media read failure"); got != 1 {
		t.Fatalf("log count = %d, want 1; output=%s", got, output)
	}
	for _, want := range []string{`"event":"stream.read_terminal_failure"`, `"outcome":"failed"`, `"failure_class":"content_missing"`, `"provider":"usenet"`, `"health_dirty_persisted":true`} {
		if !strings.Contains(output, want) {
			t.Fatalf("log missing %s: %s", want, output)
		}
	}
}

func TestRecordTerminalReadFailureIgnoresCancellation(t *testing.T) {
	m := &Manager{}
	m.RecordTerminalReadFailure(&storage.Entry{Name: "Movie.Release"}, "Movie.mkv", 0, 1, context.Canceled)
	if got := m.terminalReadFailures.Load(); got != 0 {
		t.Fatalf("terminal read failures = %d, want 0", got)
	}
}

func TestRecordTerminalReadFailureHandlesWrappedUnexpectedEOF(t *testing.T) {
	class, retryable := classifyTerminalReadFailure(errors.Join(errors.New("download failed"), io.ErrUnexpectedEOF))
	if class != "unexpected_eof" || !retryable {
		t.Fatalf("classification = %q/%v", class, retryable)
	}
}

func TestTerminalReadFailureInvalidatesPreviouslyHealthyStatus(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checked := time.Now().Add(-time.Hour).UTC()
	entry := &storage.Entry{Name: "fixture-release", InfoHash: "fixture-hash", Protocol: config.ProtocolTorrent}
	if err := store.SaveEntryHealth(&storage.EntryHealth{
		EntryName: entry.Name, Protocol: entry.Protocol, Status: storage.HealthHealthy,
		LastOKAt: checked, LastCheckedAt: checked, NextCheckDueAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{storage: store, logger: zerolog.Nop()}
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 0, 4096, customerror.TorrentNotFoundError)
	health, err := store.GetEntryHealth(entry.Name)
	if err != nil || health.Status != storage.HealthUnknown || !health.Dirty ||
		health.DirtyReason != "read_failure:content_missing" || len(health.FailedReads) != 1 ||
		!health.LastOKAt.Equal(checked) || !health.LastCheckedAt.Equal(checked) || !health.NextCheckDueAt.IsZero() {
		t.Fatalf("new read failure left misleading verified health: %+v %v", health, err)
	}
}

func TestLocalReadDeferralsDoNotInvalidateDurableProbeProgress(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry := &storage.Entry{Name: "fixture-release", InfoHash: "fixture-hash", Protocol: config.ProtocolTorrent}
	var logs bytes.Buffer
	m := &Manager{storage: store, logger: zerolog.New(&logs)}
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 0, 1024, io.ErrUnexpectedEOF)
	before, err := store.GetEntryHealth(entry.Name)
	if err != nil {
		t.Fatal(err)
	}
	before.ProbeProgress = &storage.HealthProbeProgress{Revision: before.DirtyRevision, StartedAt: time.Now(), Verified: map[string]string{"other.mkv": "proof"}}
	if err := store.SaveEntryHealth(before); err != nil {
		t.Fatal(err)
	}
	linkCooldown := link.NewLinkError(errors.New("deferred"), link.CategoryThrottled, link.CodeLinkRefreshCooldown)
	circuitCooldown := StreamError{Err: streamFileCircuitOpenError{}, Retryable: true}
	for _, failure := range []error{linkCooldown, circuitCooldown, errors.Join(linkCooldown, circuitCooldown)} {
		class, retryable := classifyTerminalReadFailure(failure)
		if class != "local_cooldown" || !retryable {
			t.Fatalf("local deferral misclassified: %s/%v", class, retryable)
		}
		m.RecordTerminalReadFailure(entry, "fixture.mkv", 1024, 1024, failure)
	}
	after, err := store.GetEntryHealth(entry.Name)
	if err != nil || after.DirtyRevision != before.DirtyRevision || len(after.FailedReads) != 1 ||
		after.ProbeProgress == nil || after.ProbeProgress.Verified["other.mkv"] != "proof" {
		t.Fatalf("local cooldown discarded real health evidence: %+v %v", after, err)
	}
	if !strings.Contains(logs.String(), `"upstream_failure_observed":false`) {
		t.Fatal("deferral was not distinguished in diagnostics")
	}
	m.RecordTerminalReadFailure(entry, "fixture.mkv", 1024, 1024, errors.Join(circuitCooldown, customerror.TorrentNotFoundError))
	after, err = store.GetEntryHealth(entry.Name)
	if err != nil || after.DirtyRevision <= before.DirtyRevision || len(after.FailedReads) != 2 || after.ProbeProgress != nil {
		t.Fatalf("mixed failover hid a newly observed failure: %+v %v", after, err)
	}
}
