// Command windows-exception-probe classifies an isolated upstream Windows
// exception reproducer. It does not start Tessarr or inspect application data.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCapture = 128 << 10

var pairPattern = regexp.MustCompile(`REPRO_PAIR span=([0-9]+) stack_lo=([0-9]+)`)
var tailPattern = regexp.MustCompile(`REPRO_TAIL before=([0-9a-f]{64}) after=([0-9a-f]{64})`)

type boundedCapture struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := min(len(p), maxCapture-b.buffer.Len())
	_, _ = b.buffer.Write(p[:keep])
	b.truncated = b.truncated || keep < len(p)
	return len(p), nil // Continue draining the child after reaching the cap.
}

type observation struct {
	Case      string `json:"case"`
	ExitCode  int    `json:"exit_code"`
	Result    string `json:"result"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func pairedTail(output string) (bool, bool) {
	pair, tail := pairPattern.FindStringSubmatch(output), tailPattern.FindStringSubmatch(output)
	if len(pair) != 3 || len(tail) != 3 {
		return false, false
	}
	span, errSpan := strconv.ParseUint(pair[1], 10, 64)
	stackLo, errStack := strconv.ParseUint(pair[2], 10, 64)
	if errSpan != nil || errStack != nil || stackLo < 8192 || stackLo-8192 != span {
		return false, false
	}
	return true, tail[1] != tail[2]
}

func classify(mode string, exitCode int, output string, timedOut, truncated bool) string {
	if timedOut {
		return "timeout"
	}
	if truncated {
		return "unexpected_failure" // A clipped trace cannot prove the result.
	}
	paired, changed := pairedTail(output)
	if !paired {
		return "unexpected_failure"
	}
	if mode == "panic" && exitCode == 0 && !changed && strings.Contains(output, "REPRO_RESULT clean") {
		return "control_passed"
	}
	if mode == "fault" {
		if exitCode == 0 && !changed && strings.Contains(output, "REPRO_RESULT clean") {
			return "not_reproduced"
		}
		if exitCode == 2 && changed &&
			strings.Contains(output, "runtime: marked free object in span") &&
			strings.Contains(output, "elemsize=16") &&
			strings.Contains(output, "fatal error: found pointer to free object") &&
			strings.Contains(output, "runtime.(*mspan).reportZombies") {
			return "exception_gc_abort"
		}
	}
	return "unexpected_failure"
}

func run(binary, mode string) observation {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, mode)
	var capture boundedCapture
	command.Stdout, command.Stderr = &capture, &capture
	err := command.Run()
	code := 0
	if err != nil {
		code = -1
		if exited, ok := err.(*exec.ExitError); ok {
			code = exited.ExitCode()
		}
	}
	output := capture.buffer.String()
	return observation{Case: mode, ExitCode: code,
		Result: classify(mode, code, output, ctx.Err() != nil, capture.truncated),
		Output: output, Truncated: capture.truncated}
}

func main() {
	binary := flag.String("binary", "", "pinned synthetic Windows exception reproducer")
	flag.Parse()
	if *binary == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: windows-exception-probe -binary <path>")
		os.Exit(2)
	}
	control, fault := run(*binary, "panic"), run(*binary, "fault")
	report := struct {
		GoVersion      string        `json:"go_version"`
		OS             string        `json:"os"`
		Arch           string        `json:"arch"`
		Reference      string        `json:"reference"`
		Interpretation string        `json:"interpretation"`
		Cases          []observation `json:"cases"`
	}{
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Reference:      "https://github.com/golang/go/issues/81238",
		Interpretation: "This independent reproducer tests one Windows exception path. A clean result on one host does not rule it out on other hosts or attribute Tessarr's earlier crash.",
		Cases:          []observation{control, fault},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encoder.Encode(report) != nil || control.Result != "control_passed" ||
		(fault.Result != "not_reproduced" && fault.Result != "exception_gc_abort") {
		os.Exit(1)
	}
}
