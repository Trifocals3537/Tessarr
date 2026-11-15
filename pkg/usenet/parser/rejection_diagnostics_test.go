package parser

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Trifocals3537/tessarr/internal/nntp"
)

func TestProbeRejectionEvidencePreservesKindsAndDecision(t *testing.T) {
	for _, test := range []struct {
		name string
		errs []error
		want RejectionReason
	}{
		{"access", []error{&nntp.Error{Type: nntp.ErrorTypeAuthentication}}, RejectionAccessDenied},
		{"timeout", []error{nntp.NewTimeoutError(errors.New("private timeout detail"))}, RejectionTimeout},
		{"wrapped timeout", []error{nntp.NewConnectionError(context.DeadlineExceeded)}, RejectionTimeout},
		{"DNS timeout without unwrap cause", []error{nntp.NewConnectionError(&net.DNSError{IsTimeout: true, Name: "private.invalid"})}, RejectionTimeout},
		{"connection", []error{nntp.NewConnectionError(errors.New("private host"))}, RejectionProviderUnavailable},
		{"mixed", []error{&nntp.Error{Type: nntp.ErrorTypeArticleNotFound}, nntp.NewTimeoutError(nil)}, RejectionMixed},
	} {
		t.Run(test.name, func(t *testing.T) {
			var failures articleProbeFailures
			for _, err := range test.errs {
				var partial articleProbeFailures
				partial.add(err)
				failures.merge(partial)
			}
			if failures.unavailableError() != nil {
				t.Fatal("diagnostics changed a non-definitive rejection to unavailable")
			}
			err := failures.rejectionError()
			if err.Error() != "no valid file groups found in NZB" || errors.Is(err, ErrNZBArticlesUnavailable) || ClassifyRejection(err) != test.want {
				t.Fatalf("probe evidence changed its message/decision or lost its reason: %v, %v", err, ClassifyRejection(err))
			}
		})
	}
}
