// Command runtime-gc-probe collects synthetic toolchain diagnostics in isolated
// child processes. It is never part of Tessarr's service startup.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const maxOutput = 64 << 10

type cappedOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, maxOutput-b.Len())
	_, _ = b.Buffer.Write(p[:keep])
	b.truncated = b.truncated || keep < n
	return n, nil // Drain the child even when the diagnostic capture is full.
}

type observation struct {
	Case      string `json:"case"`
	ExitCode  int    `json:"exit_code"`
	Result    string `json:"result"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func classify(mode string, code int, output string) string {
	if code == 0 {
		if mode == "control" {
			return "control_passed"
		}
		return "not_reproduced"
	}
	if mode == "trigger" && code == 2 {
		for _, marker := range []string{
			"fatal error: found pointer to free object",
			"fatal error: found bad pointer in Go heap",
			"fatal error: checkmark found unmarked object",
		} {
			if strings.Contains(output, marker) {
				return "runtime_gc_abort"
			}
		}
	}
	return "unexpected_failure"
}

func collect(executable, mode string) observation {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--child="+mode)
	var capture cappedOutput
	command.Stdout, command.Stderr = &capture, &capture
	err := command.Run()
	code := 0
	if err != nil {
		code = -1
		if exited, ok := err.(*exec.ExitError); ok {
			code = exited.ExitCode()
		}
	}
	result := classify(mode, code, capture.String())
	if ctx.Err() != nil {
		result = "timeout"
	}
	return observation{Case: mode, ExitCode: code, Result: result, Output: capture.String(), Truncated: capture.truncated}
}

func main() {
	if len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "--child=") {
		mode := strings.TrimPrefix(os.Args[1], "--child=")
		if mode != "control" && mode != "trigger" {
			os.Exit(64)
		}
		runChild(mode == "control")
		return
	}
	executable, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	control, trigger := collect(executable, "control"), collect(executable, "trigger")
	report := struct {
		GoVersion      string        `json:"go_version"`
		OS             string        `json:"os"`
		Arch           string        `json:"arch"`
		MaxProcs       int           `json:"max_procs"`
		GCDebug        string        `json:"gc_debug"`
		Experiment     string        `json:"experiment"`
		Reference      string        `json:"reference"`
		Interpretation string        `json:"interpretation"`
		Cases          []observation `json:"cases"`
	}{
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		MaxProcs: runtime.GOMAXPROCS(0), GCDebug: os.Getenv("GODEBUG"), Experiment: os.Getenv("GOEXPERIMENT"),
		Reference:      "https://github.com/golang/go/issues/82394",
		Interpretation: "A standalone runtime abort does not establish the cause of Tessarr's earlier CI failure. A successful diagnostic command means collection completed, not that the runtime defect is fixed.",
		Cases:          []observation{control, trigger},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		os.Exit(1)
	}
	if control.Result != "control_passed" || (trigger.Result != "runtime_gc_abort" && trigger.Result != "not_reproduced") {
		os.Exit(1)
	}
}

// This standard-library-only lifetime pattern is based on golang/go#82394.
// A >128-byte value captured by address becomes a stack object. Its data is
// deliberately dead before Goexit runs deferred collections. The control keeps
// independent strong roots for every allocation throughout the child's lifetime.
// Do not add KeepAlive(argument) to the shared function: it would also keep the
// trigger alive before collection and invalidate the comparison.
type frameArgument struct {
	data *[28]uint64
	_    [24]uint64
}

var owner *[28]uint64
var controlRoots []*[28]uint64
var observed uint64

//go:noinline
func consume(p *[28]uint64) { observed = p[0] }

//go:noinline
func invoke(fn func()) { fn() }

//go:noinline
func unwind(argument frameArgument) {
	defer func() { runtime.GC(); runtime.GC() }()
	invoke(func() { consume(argument.data) })
	owner = nil
	runtime.GC()
	runtime.GC()
	runtime.Goexit()
}

func runChild(control bool) {
	for i := uint64(0); i < 10; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := new([28]uint64)
			p[0] = i
			owner = p
			consume(owner)
			if control {
				controlRoots = append(controlRoots, p)
			}
			unwind(frameArgument{data: p})
		}()
		<-done
	}
	fmt.Println("completed", observed)
}
