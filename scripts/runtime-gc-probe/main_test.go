package main

import (
	"strings"
	"testing"
)

func TestClassificationDoesNotAcceptUnrelatedFailures(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		code         int
		output, want string
	}{
		{"control", 0, "completed", "control_passed"},
		{"trigger", 0, "completed", "not_reproduced"},
		{"trigger", 2, "fatal error: found pointer to free object", "runtime_gc_abort"},
		{"trigger", 2, "fatal error: found bad pointer in Go heap", "runtime_gc_abort"},
		{"trigger", 2, "fatal error: checkmark found unmarked object", "runtime_gc_abort"},
		{"control", 2, "fatal error: found pointer to free object", "unexpected_failure"},
		{"trigger", 1, "fatal error: found pointer to free object", "unexpected_failure"},
		{"trigger", 2, "panic: unrelated defect", "unexpected_failure"},
		{"trigger", -1, "", "unexpected_failure"},
	} {
		if got := classify(tc.mode, tc.code, tc.output); got != tc.want {
			t.Errorf("%s exit %d: %s, want %s", tc.mode, tc.code, got, tc.want)
		}
	}
}

func TestCaptureKeepsDrainingAfterItsBound(t *testing.T) {
	var capture cappedOutput
	data := []byte(strings.Repeat("x", maxOutput+100))
	for range 2 {
		if n, err := capture.Write(data); err != nil || n != len(data) {
			t.Fatalf("capture stopped draining: %d %v", n, err)
		}
	}
	if capture.Len() != maxOutput || !capture.truncated {
		t.Fatalf("unbounded capture: %d %v", capture.Len(), capture.truncated)
	}
}
