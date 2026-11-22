package parser

import (
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
)

func TestSegmentNumberingDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments nzbparser.NzbSegments
		want     string
	}{
		{"zero-based", nzbparser.NzbSegments{{Number: 1, Id: "b"}, {Number: 0, Id: "a"}}, ""},
		{"one-based", nzbparser.NzbSegments{{Number: 2, Id: "b"}, {Number: 1, Id: "a"}}, ""},
		{"none", nil, "no segments"},
		{"unexpected starting number", nzbparser.NzbSegments{{Number: 2, Id: "a"}}, "first segment is 2, expected 0 or 1"},
		{"gap", nzbparser.NzbSegments{{Number: 1, Id: "a"}, {Number: 3, Id: "b"}}, "expected segment 2, found 3"},
		{"duplicate number", nzbparser.NzbSegments{{Number: 1, Id: "a"}, {Number: 1, Id: "b"}}, "duplicate segment number 1"},
		{"empty ID", nzbparser.NzbSegments{{Number: 1}}, "empty message ID for segment 1"},
		{"duplicate ID", nzbparser.NzbSegments{{Number: 1, Id: "fixture-secret@example.invalid"}, {Number: 2, Id: "fixture-secret@example.invalid"}}, "duplicate message ID for segment 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sortAndValidateSegments(tc.segments)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.segments[0].Number >= tc.segments[1].Number {
					t.Fatal("valid segments were not sorted")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "invalid NZB segment numbering: "+tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "fixture-secret@example.invalid") {
				t.Fatalf("diagnostic disclosed a message ID: %v", err)
			}
		})
	}
}
