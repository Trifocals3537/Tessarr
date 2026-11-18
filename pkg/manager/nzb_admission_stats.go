package manager

import (
	"sync"
	"time"

	"github.com/Trifocals3537/tessarr/pkg/usenet/parser"
)

type nzbRejectionReason = parser.RejectionReason

const (
	nzbArticlesUnavailable  = parser.RejectionArticlesUnavailable
	nzbInvalidContent       = parser.RejectionInvalidContent
	nzbAccessDenied         = parser.RejectionAccessDenied
	nzbTimeout              = parser.RejectionTimeout
	nzbProviderUnavailable  = parser.RejectionProviderUnavailable
	nzbCancelled            = parser.RejectionCancelled
	nzbMixedFailure         = parser.RejectionMixed
	nzbOtherParseFailure    = parser.RejectionOther
	nzbRejectionReasonCount = parser.RejectionReasonCount
)

var nzbRejectionCodes = [nzbRejectionReasonCount]string{
	"articles_unavailable", "invalid_nzb", "access_denied", "timeout",
	"provider_unavailable", "cancelled", "mixed", "other_parse_error",
}

// NZBAdmissionStats contains process-lifetime parse rejection aggregates only.
// It never retains error strings, request identities, URLs, or payload content.
type NZBAdmissionStats struct {
	TotalRejected uint64                  `json:"total_rejected"`
	ByReason      []NZBAdmissionRejection `json:"by_reason"`
}

type NZBAdmissionRejection struct {
	Reason         string     `json:"reason"`
	Count          uint64     `json:"count"`
	LastRejectedAt *time.Time `json:"last_rejected_at,omitempty"`
}

type nzbAdmissionMetrics struct {
	mu     sync.RWMutex
	total  uint64
	counts [nzbRejectionReasonCount]uint64
	last   [nzbRejectionReasonCount]time.Time
}

// classifyNZBParseRejection considers every joined branch, with a fixed work
// budget even for cyclic or unusually large error graphs. A missing-article
// probe alone is not proof that the entire release is unavailable.
func classifyNZBParseRejection(err error) nzbRejectionReason {
	return parser.ClassifyRejection(err)
}

func (m *Manager) recordNZBParseRejection(err error) {
	if m == nil || err == nil {
		return
	}
	reason := classifyNZBParseRejection(err)
	d := &m.nzbAdmissionMetrics
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.total != ^uint64(0) {
		d.total++
	}
	if d.counts[reason] != ^uint64(0) {
		d.counts[reason]++
	}
	if now := time.Now().UTC(); now.After(d.last[reason]) {
		d.last[reason] = now
	}
}

// NZBAdmissionStats returns a detached, fixed-size view of pre-queue parsing
// rejections. Configuration resets preserve these counters; a new manager does not.
func (m *Manager) NZBAdmissionStats() NZBAdmissionStats {
	stats := NZBAdmissionStats{ByReason: make([]NZBAdmissionRejection, nzbRejectionReasonCount)}
	for i, code := range nzbRejectionCodes {
		stats.ByReason[i].Reason = code
	}
	if m == nil {
		return stats
	}
	d := &m.nzbAdmissionMetrics
	d.mu.RLock()
	defer d.mu.RUnlock()
	stats.TotalRejected = d.total
	for i := range stats.ByReason {
		stats.ByReason[i].Count = d.counts[i]
		if at := d.last[i]; !at.IsZero() {
			stats.ByReason[i].LastRejectedAt = &at
		}
	}
	return stats
}
