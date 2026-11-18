package main

import (
	"io"
	"strings"
	"sync"
	"testing"
)

const cleanTrace = "REPRO_PAIR span=8192 stack_lo=16384\nREPRO_TAIL before=" +
	"0000000000000000000000000000000000000000000000000000000000000004" +
	" after=0000000000000000000000000000000000000000000000000000000000000004\nREPRO_RESULT clean\n"

const abortTrace = "REPRO_PAIR span=8192 stack_lo=16384\nREPRO_TAIL before=" +
	"0000000000000000000000000000000000000000000000000000000000000004" +
	" after=1111111111111111111111111111111111111111111111111111111111111111\n" +
	"runtime: marked free object in span 0x2000, elemsize=16 freeindex=0\n" +
	"fatal error: found pointer to free object\nruntime.(*mspan).reportZombies(...)\n"

func TestClassificationRequiresControlAndExceptionWitness(t *testing.T) {
	for _, tc := range []struct {
		name, mode, trace, want string
		code                    int
		timedOut, truncated     bool
	}{
		{"control", "panic", cleanTrace, "control_passed", 0, false, false},
		{"clean fault", "fault", cleanTrace, "not_reproduced", 0, false, false},
		{"exact abort", "fault", abortTrace, "exception_gc_abort", 2, false, false},
		{"changed control", "panic", abortTrace, "unexpected_failure", 2, false, false},
		{"unrelated abort", "fault", strings.Replace(abortTrace, "runtime.(*mspan).reportZombies", "other.stack", 1), "unexpected_failure", 2, false, false},
		{"missing adjacency", "fault", strings.Replace(abortTrace, "span=8192", "span=8000", 1), "unexpected_failure", 2, false, false},
		{"changed tail without abort", "fault", strings.Replace(abortTrace, "fatal error: found pointer to free object", "REPRO_RESULT clean", 1), "unexpected_failure", 0, false, false},
		{"timeout", "fault", cleanTrace, "timeout", -1, true, false},
		{"truncated", "fault", abortTrace, "unexpected_failure", 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.mode, tc.code, tc.trace, tc.timedOut, tc.truncated); got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCaptureBoundsPipeCopies(t *testing.T) {
	var capture boundedCapture
	data := strings.Repeat("x", maxCapture+100)
	reader := struct{ io.Reader }{strings.NewReader(data)}
	if n, err := io.Copy(&capture, reader); err != nil || n != int64(len(data)) {
		t.Fatalf("pipe was not drained: %d %v", n, err)
	}
	if capture.buffer.Len() != maxCapture || !capture.truncated {
		t.Fatalf("capture limit bypassed: %d %v", capture.buffer.Len(), capture.truncated)
	}
}

func TestCaptureBoundsConcurrentChildStreams(t *testing.T) {
	var capture boundedCapture
	var writers sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for range 10 {
				if n, err := capture.Write([]byte(strings.Repeat("x", 4096))); err != nil || n != 4096 {
					t.Errorf("child stream was not drained: %d %v", n, err)
				}
			}
		})
	}
	writers.Wait()
	if capture.buffer.Len() != maxCapture || !capture.truncated {
		t.Fatalf("concurrent capture limit bypassed: %d %v", capture.buffer.Len(), capture.truncated)
	}
}
