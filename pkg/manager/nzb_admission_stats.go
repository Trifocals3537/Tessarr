package manager

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/usenet/parser"
)

type nzbRejectionReason uint8

const (
	nzbArticlesUnavailable nzbRejectionReason = iota
	nzbInvalidContent
	nzbAccessDenied
	nzbTimeout
	nzbProviderUnavailable
	nzbCancelled
	nzbMixedFailure
	nzbOtherParseFailure
	nzbRejectionReasonCount
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
	bit := func(reason nzbRejectionReason) uint16 { return 1 << reason }
	remaining := 64
	var visit func(error) uint16
	visit = func(current error) uint16 {
		if remaining <= 0 {
			return bit(nzbOtherParseFailure)
		}
		remaining--
		if current == nil {
			return 0
		}
		switch typed := current.(type) {
		case *parser.InvalidNZBError:
			return bit(nzbInvalidContent)
		case *parser.ArticlesUnavailableError:
			return bit(nzbArticlesUnavailable)
		case *nntp.Error:
			switch typed.Type {
			case nntp.ErrorTypeAuthentication, nntp.ErrorTypePermissionDenied:
				return bit(nzbAccessDenied)
			case nntp.ErrorTypeTimeout:
				return bit(nzbTimeout)
			case nntp.ErrorTypeConnection, nntp.ErrorTypeServerBusy, nntp.ErrorTypeNoAvailableConnection:
				return bit(nzbProviderUnavailable)
			default:
				return bit(nzbOtherParseFailure)
			}
		}
		switch current {
		case parser.ErrNZBArticlesUnavailable:
			return bit(nzbArticlesUnavailable)
		case context.Canceled:
			return bit(nzbCancelled)
		case context.DeadlineExceeded:
			return bit(nzbTimeout)
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			var reasons uint16
			for _, child := range joined.Unwrap() {
				if remaining <= 0 {
					return reasons | bit(nzbOtherParseFailure)
				}
				reasons |= visit(child)
			}
			return reasons
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		if timeout, ok := current.(net.Error); ok && timeout.Timeout() {
			return bit(nzbTimeout)
		}
		return bit(nzbOtherParseFailure)
	}
	mask := visit(err)
	if mask == 0 {
		return nzbOtherParseFailure
	}
	if mask&(mask-1) != 0 {
		return nzbMixedFailure
	}
	for reason := nzbRejectionReason(0); reason < nzbRejectionReasonCount; reason++ {
		if mask == bit(reason) {
			return reason
		}
	}
	return nzbOtherParseFailure
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
