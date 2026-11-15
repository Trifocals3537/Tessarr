package parser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestInvalidNZBMarkerPreservesMessageAndCause(t *testing.T) {
	cause := errors.New("original validation message")
	err := NewInvalidNZBError(cause)
	if err.Error() != cause.Error() || !errors.Is(err, cause) || NewInvalidNZBError(nil) != nil {
		t.Fatal("invalid-input marker changed the original error contract")
	}
}

func TestMalformedNZBXMLHasTypedDiagnosticMarker(t *testing.T) {
	_, _, err := NewParser(nil, 1, zerolog.Nop()).Parse(context.Background(), "Synthetic.nzb", []byte(`<nzb><file>`))
	var invalid *InvalidNZBError
	if !errors.As(err, &invalid) || invalid.Unwrap() == nil || !strings.HasPrefix(err.Error(), "failed to parse NZB content: ") || !strings.Contains(invalid.Error(), "XML syntax error") {
		t.Fatalf("malformed XML lost its marker, original cause, or error prefix: %v", err)
	}
}
