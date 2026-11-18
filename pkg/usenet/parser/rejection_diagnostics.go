package parser

import (
	"context"
	"net"

	"github.com/Trifocals3537/tessarr/internal/nntp"
)

// RejectionReason is a fixed diagnostic vocabulary, not an admission policy.
type RejectionReason uint8

const (
	RejectionArticlesUnavailable RejectionReason = iota
	RejectionInvalidContent
	RejectionAccessDenied
	RejectionTimeout
	RejectionProviderUnavailable
	RejectionCancelled
	RejectionMixed
	RejectionOther
	RejectionReasonCount
)

// probeFailureError preserves only bounded category evidence when content
// detection produces no groups. Its message and lack of an unavailable marker
// match the previous generic rejection; it retains no individual probe errors.
type probeFailureError struct{ kinds uint16 }

func (*probeFailureError) Error() string { return "no valid file groups found in NZB" }

func rejectionBit(reason RejectionReason) uint16 { return 1 << reason }

// rejectionMask inspects joined branches under one fixed traversal budget.
// Explicit semantic markers own their causes; connection wrappers additionally
// preserve a more specific timeout, cancellation, or access failure underneath.
func rejectionMask(err error) uint16 {
	remaining := 64
	var visit func(error) uint16
	visit = func(current error) uint16 {
		if remaining <= 0 {
			return rejectionBit(RejectionOther)
		}
		remaining--
		if current == nil {
			return 0
		}
		switch typed := current.(type) {
		case *InvalidNZBError:
			return rejectionBit(RejectionInvalidContent)
		case *ArticlesUnavailableError:
			return rejectionBit(RejectionArticlesUnavailable)
		case *probeFailureError:
			return typed.kinds
		case *nntp.Error:
			switch typed.Type {
			case nntp.ErrorTypeAuthentication, nntp.ErrorTypePermissionDenied:
				return rejectionBit(RejectionAccessDenied)
			case nntp.ErrorTypeTimeout:
				return rejectionBit(RejectionTimeout)
			case nntp.ErrorTypeConnection, nntp.ErrorTypeServerBusy, nntp.ErrorTypeNoAvailableConnection:
				cause := visit(typed.Err)
				fallback := rejectionBit(RejectionProviderUnavailable)
				if cause == 0 || cause == rejectionBit(RejectionOther) {
					return fallback
				}
				if cause&(rejectionBit(RejectionArticlesUnavailable)|rejectionBit(RejectionInvalidContent)) != 0 {
					return cause | fallback
				}
				return cause
			default:
				// One missing article alone is not release-level unavailability.
				return rejectionBit(RejectionOther)
			}
		}
		switch current {
		case ErrNZBArticlesUnavailable:
			return rejectionBit(RejectionArticlesUnavailable)
		case context.Canceled:
			return rejectionBit(RejectionCancelled)
		case context.DeadlineExceeded:
			return rejectionBit(RejectionTimeout)
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			var kinds uint16
			for _, child := range joined.Unwrap() {
				if remaining <= 0 {
					return kinds | rejectionBit(RejectionOther)
				}
				kinds |= visit(child)
			}
			return kinds
		}
		if timeout, ok := current.(net.Error); ok && timeout.Timeout() {
			return rejectionBit(RejectionTimeout)
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return rejectionBit(RejectionOther)
	}
	mask := visit(err)
	if mask == 0 && err != nil {
		return rejectionBit(RejectionOther)
	}
	return mask
}

// ClassifyRejection returns secret-free evidence for diagnostics. Admission
// decisions must continue using the existing explicit error contracts.
func ClassifyRejection(err error) RejectionReason {
	mask := rejectionMask(err)
	if mask == 0 {
		return RejectionOther
	}
	if mask&(mask-1) != 0 {
		return RejectionMixed
	}
	for reason := RejectionReason(0); reason < RejectionReasonCount; reason++ {
		if mask == rejectionBit(reason) {
			return reason
		}
	}
	return RejectionOther
}
