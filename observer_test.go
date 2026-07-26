package sonnetbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type recordedEvents struct {
	mu           sync.Mutex
	imports      []ImportEvent
	capabilities []CapabilityEvent
	evaluations  []EvaluationEvent
}

func (r *recordedEvents) observer() *Observer {
	return &Observer{
		Import: func(_ context.Context, event ImportEvent) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.imports = append(r.imports, event)
		},
		Capability: func(_ context.Context, event CapabilityEvent) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.capabilities = append(r.capabilities, event)
		},
		Evaluation: func(_ context.Context, event EvaluationEvent) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.evaluations = append(r.evaluations, event)
		},
	}
}

func (r *recordedEvents) snapshot() ([]ImportEvent, []CapabilityEvent, []EvaluationEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.imports, r.capabilities, r.evaluations
}

func newObservedEngine(t *testing.T, recorded *recordedEvents) *Engine {
	t.Helper()
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithObserver(recorded.observer()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	return engine
}

func TestObserverRecordsSuccessfulActivity(t *testing.T) {
	recorded := &recordedEvents{}
	engine := newObservedEngine(t, recorded)

	importer, err := NewMapImporter(map[string][]byte{
		"lib.libsonnet": []byte(`{value: 1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(context.Background(), Request{
		Filename: "app.jsonnet",
		Source:   `(import "lib.libsonnet") + {doubled: std.native("double")(21)}`,
		Importer: importer,
		Capabilities: map[string]Capability{
			"double": {Params: []string{"value"}, Call: func(_ context.Context, args []any) (any, error) {
				return args[0].(float64) * 2, nil //nolint:forcetypeassert // the test controls the argument.
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	imports, capabilities, evaluations := recorded.snapshot()
	if len(imports) != 1 {
		t.Fatalf("import events = %d, want 1", len(imports))
	}
	if imports[0].ImportedPath != "lib.libsonnet" ||
		imports[0].ResolvedPath != "lib.libsonnet" ||
		imports[0].Bytes != len(`{value: 1}`) ||
		imports[0].Denied ||
		imports[0].Err != nil {
		t.Fatalf("import event = %#v", imports[0])
	}
	if len(capabilities) != 1 {
		t.Fatalf("capability events = %d, want 1", len(capabilities))
	}
	if capabilities[0].Name != "double" || capabilities[0].Args != 1 || capabilities[0].Err != nil {
		t.Fatalf("capability event = %#v", capabilities[0])
	}
	if len(evaluations) != 1 {
		t.Fatalf("evaluation events = %d, want 1", len(evaluations))
	}
	if evaluations[0].Filename != "app.jsonnet" ||
		evaluations[0].Err != nil ||
		evaluations[0].Stats.ImportResolutions != 1 ||
		evaluations[0].Stats.CapabilityCalls != 1 {
		t.Fatalf("evaluation event = %#v", evaluations[0])
	}
}

func TestObserverRecordsDeniedImport(t *testing.T) {
	recorded := &recordedEvents{}
	engine := newObservedEngine(t, recorded)

	if _, err := engine.Evaluate(context.Background(), Request{
		Source: `import "forbidden.libsonnet"`,
	}); err == nil {
		t.Fatal("expected the import to be denied")
	}

	imports, _, evaluations := recorded.snapshot()
	if len(imports) != 1 {
		t.Fatalf("import events = %d, want 1", len(imports))
	}
	if !imports[0].Denied || imports[0].Err == nil || imports[0].ResolvedPath != "" {
		t.Fatalf("import event = %#v", imports[0])
	}
	if len(evaluations) != 1 || evaluations[0].Err == nil {
		t.Fatalf("evaluation events = %#v", evaluations)
	}
}

func TestObserverRecordsFailedCapabilityAndEvaluation(t *testing.T) {
	recorded := &recordedEvents{}
	engine := newObservedEngine(t, recorded)

	if _, err := engine.Evaluate(context.Background(), Request{
		Source: `std.native("broken")()`,
		Capabilities: map[string]Capability{
			"broken": {Call: func(context.Context, []any) (any, error) {
				return nil, errors.New("handler said no")
			}},
		},
	}); err == nil {
		t.Fatal("expected the capability failure to surface")
	}

	_, capabilities, evaluations := recorded.snapshot()
	if len(capabilities) != 1 || capabilities[0].Err == nil {
		t.Fatalf("capability events = %#v", capabilities)
	}
	if len(evaluations) != 1 || evaluations[0].Err == nil {
		t.Fatalf("evaluation events = %#v", evaluations)
	}
}

func TestObserverSeesRequestsRejectedBeforeTheGuest(t *testing.T) {
	recorded := &recordedEvents{}
	engine := newObservedEngine(t, recorded)

	if _, err := engine.Evaluate(context.Background(), Request{
		Source: string([]byte{0xff, 0xfe}),
	}); err == nil {
		t.Fatal("expected invalid UTF-8 to be rejected")
	}

	_, _, evaluations := recorded.snapshot()
	if len(evaluations) != 1 || evaluations[0].Err == nil {
		t.Fatalf("a request rejected during validation must still be observed: %#v", evaluations)
	}
}

func TestSlogObserverLogsDenialsAndActivity(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine, err := NewEngine(
		context.Background(),
		EngineConfig{},
		WithObserver(NewSlogObserver(logger)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	if _, err := engine.Evaluate(context.Background(), Request{
		Source: `import "forbidden.libsonnet"`,
	}); err == nil {
		t.Fatal("expected the import to be denied")
	}

	var sawDenial bool
	for line := range strings.SplitSeq(strings.TrimSpace(logged.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if entry["msg"] == "sonnetbox import denied" {
			if entry["level"] != "WARN" {
				t.Fatalf("a denial must be logged at warn level: %s", line)
			}
			if entry["imported_path"] != "forbidden.libsonnet" {
				t.Fatalf("denial log is missing the path: %s", line)
			}
			sawDenial = true
		}
	}
	if !sawDenial {
		t.Fatalf("no denial was logged:\n%s", logged.String())
	}
}

func TestSlogObserverFallsBackToTheDefaultLogger(t *testing.T) {
	if NewSlogObserver(nil) == nil {
		t.Fatal("NewSlogObserver(nil) must still return an observer")
	}
}

func TestNilObserverIsRejected(t *testing.T) {
	_, err := NewEngine(context.Background(), EngineConfig{}, WithObserver(nil))
	var invalid *InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
	}
}

func TestPartialObserverIsSafe(t *testing.T) {
	var seen int
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithObserver(&Observer{
		Evaluation: func(context.Context, EvaluationEvent) { seen++ },
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	// The unset Import hook must simply be skipped.
	if _, err := engine.Evaluate(context.Background(), Request{
		Source: `import "forbidden.libsonnet"`,
	}); err == nil {
		t.Fatal("expected the import to be denied")
	}
	if seen != 1 {
		t.Fatalf("evaluation hook ran %d times, want 1", seen)
	}
}
