package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/internal/utils"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/parser"
)

func TestNZBRejectionClassificationUsesTypedEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want nzbRejectionReason
	}{
		{"all articles unavailable", parser.ErrNZBArticlesUnavailable, nzbArticlesUnavailable},
		{"wrapped unavailable", fmt.Errorf("private title: %w", parser.ErrNZBArticlesUnavailable), nzbArticlesUnavailable},
		{"individual article is not release evidence", &nntp.Error{Type: nntp.ErrorTypeArticleNotFound}, nzbOtherParseFailure},
		{"invalid input", parser.NewInvalidNZBError(errors.New("private HTML body")), nzbInvalidContent},
		{"authentication", &nntp.Error{Type: nntp.ErrorTypeAuthentication, Err: errors.New("private credentials")}, nzbAccessDenied},
		{"permission", &nntp.Error{Type: nntp.ErrorTypePermissionDenied}, nzbAccessDenied},
		{"timeout", nntp.NewTimeoutError(errors.New("private endpoint")), nzbTimeout},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), nzbTimeout},
		{"connection", nntp.NewConnectionError(errors.New("private host")), nzbProviderUnavailable},
		{"connection-wrapped socket timeout", nntp.NewConnectionError(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}), nzbTimeout},
		{"busy", &nntp.Error{Type: nntp.ErrorTypeServerBusy}, nzbProviderUnavailable},
		{"pool unavailable", &nntp.Error{Type: nntp.ErrorTypeNoAvailableConnection}, nzbProviderUnavailable},
		{"cancelled", fmt.Errorf("wrapped: %w", context.Canceled), nzbCancelled},
		{"error text is not evidence", errors.New("NZB articles unavailable: invalid NZB timeout password"), nzbOtherParseFailure},
		{"two missing branches", errors.Join(parser.ErrNZBArticlesUnavailable, parser.ErrNZBArticlesUnavailable), nzbArticlesUnavailable},
		{"mixed missing and timeout", errors.Join(parser.ErrNZBArticlesUnavailable, nntp.NewTimeoutError(nil)), nzbMixedFailure},
		{"mixed missing and access", errors.Join(parser.ErrNZBArticlesUnavailable, &nntp.Error{Type: nntp.ErrorTypeAuthentication}), nzbMixedFailure},
		{"mixed missing and unknown", errors.Join(parser.ErrNZBArticlesUnavailable, errors.New("other failure")), nzbMixedFailure},
		{"mixed invalid and timeout", errors.Join(parser.NewInvalidNZBError(errors.New("body")), context.DeadlineExceeded), nzbMixedFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyNZBParseRejection(test.err); got != test.want {
				t.Fatalf("reason = %q, want %q", nzbRejectionCodes[got], nzbRejectionCodes[test.want])
			}
		})
	}
}

type cyclicNZBError struct{ visits int }

func (*cyclicNZBError) Error() string { panic("diagnostics must not format errors") }
func (e *cyclicNZBError) Unwrap() error {
	e.visits++
	return e
}

type mixedNZBMarker struct{}

func (*mixedNZBMarker) Error() string        { panic("diagnostics must not format errors") }
func (*mixedNZBMarker) Is(target error) bool { return target == parser.ErrNZBArticlesUnavailable }
func (*mixedNZBMarker) Unwrap() []error {
	return []error{parser.ErrNZBArticlesUnavailable, context.DeadlineExceeded}
}

func TestNZBRejectionClassificationBoundsErrorGraphs(t *testing.T) {
	if got := classifyNZBParseRejection(&mixedNZBMarker{}); got != nzbMixedFailure {
		t.Fatal("a broad Is marker concealed conflicting error branches")
	}
	cycle := &cyclicNZBError{}
	if got := classifyNZBParseRejection(cycle); got != nzbOtherParseFailure || cycle.visits > 64 {
		t.Fatalf("cyclic error reason=%d visits=%d", got, cycle.visits)
	}
	branches := make([]error, 1000)
	for i := range branches {
		branches[i] = parser.ErrNZBArticlesUnavailable
	}
	if got := classifyNZBParseRejection(errors.Join(branches...)); got == nzbArticlesUnavailable {
		t.Fatal("truncated error graph was presented as definitive missing-article evidence")
	}
}

func TestNZBRejectionSnapshotsAreBoundedDetachedAndSecretFree(t *testing.T) {
	m := &Manager{}
	secret := "Secret.Title https://user:password@private.invalid/path?apikey=private-key article@private.invalid C:/private/media"
	for range 1000 {
		m.recordNZBParseRejection(nntp.NewTimeoutError(errors.New(secret)))
	}
	m.recordNZBParseRejection(parser.NewInvalidNZBError(errors.New(secret)))
	m.recordNZBParseRejection(nil)
	before := m.NZBAdmissionStats()
	data, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if before.TotalRejected != 1001 || len(before.ByReason) != 8 || len(data) > 2048 {
		t.Fatalf("unbounded or incorrect summary: %s", data)
	}
	for _, private := range []string{"Secret.Title", "password", "private.invalid", "private-key", "article@", "C:/private", "https://"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("diagnostics exposed %q", private)
		}
	}
	for i, reason := range before.ByReason {
		if reason.Count == 0 {
			continue
		}
		if reason.LastRejectedAt == nil || reason.LastRejectedAt.Location() != time.UTC {
			t.Fatal("rejection timestamp is absent or not UTC")
		}
		originalTime := *reason.LastRejectedAt
		before.ByReason[i].Reason = secret
		before.ByReason[i].Count = 0
		*before.ByReason[i].LastRejectedAt = time.Time{}
		after := m.NZBAdmissionStats().ByReason[i]
		if after.Reason != nzbRejectionCodes[i] || after.Count != reason.Count || !after.LastRejectedAt.Equal(originalTime) {
			t.Fatal("a caller mutated retained diagnostics through a snapshot")
		}
	}
	if fresh := (&Manager{}).NZBAdmissionStats(); fresh.TotalRejected != 0 || len(fresh.ByReason) != 8 {
		t.Fatal("a new manager inherited another manager's process counters")
	}
}

func TestNZBRejectionCountersAreConcurrentAndSaturating(t *testing.T) {
	m := &Manager{}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 100 {
				m.recordNZBParseRejection(parser.ErrNZBArticlesUnavailable)
				snapshot := m.NZBAdmissionStats()
				var sum uint64
				for _, reason := range snapshot.ByReason {
					sum += reason.Count
				}
				if sum != snapshot.TotalRejected {
					t.Error("snapshot combined different counter generations")
					return
				}
			}
		})
	}
	workers.Wait()
	if got := m.NZBAdmissionStats().TotalRejected; got != 1600 {
		t.Fatalf("concurrent total = %d, want 1600", got)
	}
	m.nzbAdmissionMetrics.total = ^uint64(0)
	m.nzbAdmissionMetrics.counts[nzbTimeout] = ^uint64(0)
	m.recordNZBParseRejection(context.DeadlineExceeded)
	if m.nzbAdmissionMetrics.total != ^uint64(0) || m.nzbAdmissionMetrics.counts[nzbTimeout] != ^uint64(0) {
		t.Fatal("rejection counters wrapped to zero")
	}
}

func TestNZBAdmissionRecordsParseRejectionsBeforeQueue(t *testing.T) {
	for _, test := range []struct {
		name, content string
		stat          int
		want          string
	}{
		{"missing articles", recoveryNameNZB, 430, "articles_unavailable"},
		{"HTML with secrets", `<html>private-key Secret.Title article@private.invalid</html>`, 223, "invalid_nzb"},
		{"malformed XML", `<nzb><file subject="Secret.Title">`, 223, "invalid_nzb"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newNZBRecoveryNameTestManager(t, nzbRecoveryFixtureOptions{statCode: test.stat})
			req := NewNZBRequest("Synthetic.nzb", m.config.DownloadFolder, []byte(test.content), m.arr.GetOrCreate("tv"), config.DownloadActionDownload, "", ImportTypeSABnzbd, true)
			id, err := m.AddNewNZB(m.ctx, req)
			if err == nil || id != "" {
				t.Fatalf("rejected content was admitted: id=%q error=%v", id, err)
			}
			if _, lookupErr := m.queue.GetTorrent(req.Id); !storage.IsQueuedEntryNotFound(lookupErr) {
				t.Fatalf("rejection created a queue record: %v", lookupErr)
			}
			if m.jobQueue.Len() != 0 || m.jobQueue.ActiveCount() != 0 {
				t.Fatal("rejected content reached a worker")
			}
			stats := m.NZBAdmissionStats()
			if stats.TotalRejected != 1 {
				t.Fatalf("one rejection was not recorded exactly once: %+v", stats)
			}
			for _, reason := range stats.ByReason {
				if reason.Count != 0 && (reason.Reason != test.want || reason.Count != 1 || reason.LastRejectedAt == nil) {
					t.Fatalf("incorrect rejection classification: %+v", reason)
				}
			}
		})
	}
}

func TestNZBSuccessfulAdmissionDoesNotRecordRejection(t *testing.T) {
	m := newNZBRecoveryNameTestManager(t)
	admitRecoveryNameNZB(t, m, "Synthetic.nzb")
	if stats := m.NZBAdmissionStats(); stats.TotalRejected != 0 {
		t.Fatalf("successful admission counted as a rejection: %+v", stats)
	}
}

func TestNZBRedirectResponsesKeepInputClassification(t *testing.T) {
	for _, test := range []struct {
		name, content string
		accepted      bool
	}{
		{"valid generic binary", recoveryNameNZB, true},
		{"HTML with HTTP 200", `<html>Secret.Title private-key</html>`, false},
		{"Newznab error XML", `<error code="100" description="private-key"/>`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newNZBRecoveryNameTestManager(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/payload?apikey=private-key", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(test.content))
			}))
			defer server.Close()
			filename, content, err := utils.DownloadFileBounded(m.ctx, server.URL+"/redirect", utils.MaxMetadataFileBytes)
			if err != nil {
				t.Fatal(err)
			}
			req := NewNZBRequest(filename, m.config.DownloadFolder, content, m.arr.GetOrCreate("tv"), config.DownloadActionDownload, "", ImportTypeAPI, true)
			id, err := m.AddNewNZB(m.ctx, req)
			if (err == nil) != test.accepted || (id != "") != test.accepted || requests.Load() != 2 {
				t.Fatalf("redirect admission: id=%q accepted=%t requests=%d error=%v", id, test.accepted, requests.Load(), err)
			}
			stats := m.NZBAdmissionStats()
			if test.accepted {
				if stats.TotalRejected != 0 {
					t.Fatal("valid binary NZB was counted as rejected")
				}
			} else if stats.TotalRejected != 1 || stats.ByReason[nzbInvalidContent].Count != 1 {
				t.Fatal("non-NZB redirect body was not classified as invalid input")
			}
		})
	}
}

func TestNZBObfuscatedAdmissionPreservesFailureKinds(t *testing.T) {
	obfuscated := strings.Replace(recoveryNameNZB, "payload.mkv", "opaque-subject", 1)
	for _, test := range []struct {
		name, content string
		bodyCodes     map[string]int
		want          string
	}{
		{"access denied", obfuscated, map[string]int{"payload@recovery.invalid": 481}, "access_denied"},
		{"provider unavailable", obfuscated, map[string]int{"payload@recovery.invalid": 503}, "provider_unavailable"},
		{"mixed", strings.Replace(obfuscated, "</nzb>", `<file poster="fixture" date="1" subject="&quot;second-opaque-subject&quot; yEnc (1/1)"><groups><group>alt.test</group></groups><segments><segment bytes="1024" number="1">second@recovery.invalid</segment></segments></file></nzb>`, 1), map[string]int{"payload@recovery.invalid": 481, "second@recovery.invalid": 430}, "mixed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newNZBRecoveryNameTestManager(t, nzbRecoveryFixtureOptions{bodyCodes: test.bodyCodes})
			req := NewNZBRequest("Synthetic.nzb", m.config.DownloadFolder, []byte(test.content), m.arr.GetOrCreate("tv"), config.DownloadActionDownload, "", ImportTypeSABnzbd, true)
			id, err := m.AddNewNZB(m.ctx, req)
			if err == nil || id != "" || err.Error() != "usenet parse failed: no valid file groups found in NZB" {
				t.Fatalf("obfuscated rejection decision/message changed: id=%q error=%v", id, err)
			}
			if _, lookupErr := m.queue.GetTorrent(req.Id); !storage.IsQueuedEntryNotFound(lookupErr) {
				t.Fatalf("obfuscated rejection entered the queue: %v", lookupErr)
			}
			stats := m.NZBAdmissionStats()
			if stats.TotalRejected != 1 {
				t.Fatalf("rejection total = %d, want 1", stats.TotalRejected)
			}
			found := false
			for _, reason := range stats.ByReason {
				if reason.Count != 0 {
					found = reason.Reason == test.want && reason.Count == 1
				}
			}
			if !found {
				t.Fatalf("obfuscated admission lost %q failure evidence: %+v", test.want, stats)
			}
		})
	}
}
