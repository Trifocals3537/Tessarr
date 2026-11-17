package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/workflow"
	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/rs/zerolog"
)

type diagnosticLegacyProvider struct {
	*legacyRecoveryProvider
	log zerolog.Logger
}

func (p *diagnosticLegacyProvider) Logger() zerolog.Logger { return p.log }

type legacyPanicLog struct {
	mu   sync.Mutex
	data []byte
	once sync.Once
	done chan struct{}
}

func (w *legacyPanicLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.data = append(w.data, data...)
	w.mu.Unlock()
	w.once.Do(func() { close(w.done) })
	return len(data), nil
}

func (w *legacyPanicLog) snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.data)
}

func readLegacyDiagnostic(ctx context.Context, provider *diagnosticLegacyProvider, operation string) error {
	if operation == "get_torrent" {
		_, err := getProviderTorrent(ctx, provider, "fixture-private-transfer-id")
		return err
	}
	_, err := getProviderTorrents(ctx, provider)
	return err
}

func assertLegacyPanicDiagnostic(t *testing.T, data []byte, operation string) {
	t.Helper()
	var event struct {
		Level     string `json:"level"`
		Operation string `json:"operation"`
		Stack     string `json:"stack"`
		Message   string `json:"message"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&event); err != nil {
		t.Fatalf("panic diagnostic missing or invalid: %v", err)
	}
	if event.Level != "error" || event.Operation != operation || event.Message != "Legacy provider read panicked" ||
		!strings.Contains(event.Stack, "provider_panic_diagnostics_test.go") {
		t.Fatalf("panic diagnostic lacks its operation or originating stack: %+v", event)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected one panic diagnostic, got additional output: %v", err)
	}
	for _, secret := range []string{"fixture-private-transfer-id", "fixture-private-signed-url", "do not format provider secrets"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("panic diagnostic included a private value")
		}
	}
}

func TestLegacyProviderReadPreservesSanitizedPanicDiagnostic(t *testing.T) {
	for _, operation := range []string{"get_torrent", "list_torrents"} {
		for _, payload := range []struct {
			name  string
			value any
		}{
			{"string", "fixture-private-signed-url"},
			{"unformattable", unsafeRecoveryPanic{}},
		} {
			t.Run(operation+"/"+payload.name, func(t *testing.T) {
				capture := &legacyPanicLog{done: make(chan struct{})}
				provider := &diagnosticLegacyProvider{
					legacyRecoveryProvider: &legacyRecoveryProvider{routingTestClient: &routingTestClient{}},
					log:                    zerolog.New(capture),
				}
				provider.get = func(string) (*debridTypes.Torrent, error) { panic(payload.value) }
				provider.list = func() ([]*debridTypes.Torrent, error) { panic(payload.value) }
				if err := readLegacyDiagnostic(t.Context(), provider, operation); err != workflow.ErrPanicked {
					t.Fatalf("panic result changed: %v", err)
				}
				assertLegacyPanicDiagnostic(t, capture.snapshot(), operation)
			})
		}
	}
}

func TestCanceledLegacyProviderReadRetainsLatePanicDiagnostic(t *testing.T) {
	for _, operation := range []string{"get_torrent", "list_torrents"} {
		t.Run(operation, func(t *testing.T) {
			capture := &legacyPanicLog{done: make(chan struct{})}
			provider := &diagnosticLegacyProvider{
				legacyRecoveryProvider: &legacyRecoveryProvider{routingTestClient: &routingTestClient{}},
				log:                    zerolog.New(capture),
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			work := func() {
				close(entered)
				<-release
				panic("fixture-private-signed-url")
			}
			provider.get = func(string) (*debridTypes.Torrent, error) { work(); return nil, nil }
			provider.list = func() ([]*debridTypes.Torrent, error) { work(); return nil, nil }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			returned := make(chan error, 1)
			go func() { returned <- readLegacyDiagnostic(ctx, provider, operation) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("legacy read did not start")
			}
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation result changed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled caller waited for the legacy provider")
			}
			unblock()
			select {
			case <-capture.done:
			case <-time.After(5 * time.Second):
				t.Fatal("late provider panic lost its diagnostic")
			}
			assertLegacyPanicDiagnostic(t, capture.snapshot(), operation)
		})
	}
}
