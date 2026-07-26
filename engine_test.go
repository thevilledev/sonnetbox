package sonnetbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jsonnet "github.com/google/go-jsonnet"
	"github.com/thevilledev/sonnetbox/internal/protocol"
	"github.com/thevilledev/wazero/experimental/wazerotest"
)

func TestEvaluateBasicAndJsonnetError(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		Source: `{answer: 6 * 7}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"answer": float64(42)})

	_, err = engine.Evaluate(context.Background(), Request{Source: `error "nope"`})
	var evaluationErr *EvaluationError
	if !errors.As(err, &evaluationErr) {
		t.Fatalf("expected EvaluationError, got %T: %v", err, err)
	}

	_, err = engine.Evaluate(context.Background(), Request{Source: `local =`})
	if !errors.As(err, &evaluationErr) {
		t.Fatalf("expected static EvaluationError, got %T: %v", err, err)
	}
}

func TestEvaluateVariablesAndArguments(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		Source:  `function(a, b) {ext: std.extVar("plain"), code: std.extVar("code"), tlaVar: a, tlaCode: b}`,
		ExtVars: map[string]string{"plain": "value"},
		ExtCode: map[string]string{"code": `{nested: true}`},
		TLAVars: map[string]string{"a": "20"},
		TLACode: map[string]string{"b": "20 + 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{
		"ext":     "value",
		"code":    map[string]any{"nested": true},
		"tlaVar":  "20",
		"tlaCode": float64(22),
	})
}

func TestStringOutputAndTrailingNewline(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		Source:              `"hello"`,
		StringOutput:        true,
		OmitTrailingNewline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != "hello" {
		t.Fatalf("unexpected string output %q", result.Output)
	}

	result, err = engine.Evaluate(context.Background(), Request{
		Source:       `"hello"`,
		StringOutput: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != "hello\n" {
		t.Fatalf("unexpected newline output %q", result.Output)
	}
}

func TestMultiAndStreamOutput(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	multi, err := engine.Evaluate(context.Background(), Request{
		Source:     `{["a.json"]: {value: 1}, ["b.json"]: {value: 2}}`,
		OutputMode: OutputModeMulti,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(multi.Files) != 2 {
		t.Fatalf("multi-file outputs = %d, want 2", len(multi.Files))
	}
	assertJSON(t, multi.Files["a.json"], map[string]any{"value": float64(1)})
	assertJSON(t, multi.Files["b.json"], map[string]any{"value": float64(2)})

	stream, err := engine.Evaluate(context.Background(), Request{
		Source:     `[{value: 1}, {value: 2}]`,
		OutputMode: OutputModeStream,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stream.Documents) != 2 {
		t.Fatalf("stream documents = %d, want 2", len(stream.Documents))
	}
	assertJSON(t, stream.Documents[0], map[string]any{"value": float64(1)})
	assertJSON(t, stream.Documents[1], map[string]any{"value": float64(2)})
}

func TestEngineConfigValidation(t *testing.T) {
	got, err := normalizeConfig(EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultConfig {
		t.Fatalf("defaults = %#v, want %#v", got, defaultConfig)
	}

	for _, test := range []struct {
		name   string
		config EngineConfig
	}{
		{
			name:   "memory alignment",
			config: EngineConfig{MaxMemoryBytes: wasmPageSize + 1},
		},
		{
			name:   "negative stack",
			config: EngineConfig{MaxStack: -1},
		},
		{
			name:   "memory ceiling",
			config: EngineConfig{MaxMemoryBytes: hardCeilings.MaxMemoryBytes + wasmPageSize},
		},
		{
			name:   "fuel ceiling",
			config: EngineConfig{MaxFuel: hardCeilings.MaxFuel + 1},
		},
		{
			name: "WASM stack ceiling",
			config: EngineConfig{
				MaxWasmStackBytes: hardCeilings.MaxWasmStackBytes + 1,
			},
		},
		{
			name:   "request ceiling",
			config: EngineConfig{MaxHostRequestBytes: hardCeilings.MaxHostRequestBytes + 1},
		},
		{
			name:   "response minimum",
			config: EngineConfig{MaxHostResponseBytes: minHostResponseSize - 1},
		},
		{
			name: "trace ceiling",
			config: EngineConfig{
				MaxTraceBytes: hardCeilings.MaxTraceBytes + 1,
			},
		},
		{
			name: "concurrency ceiling",
			config: EngineConfig{
				MaxConcurrentEvaluations: hardCeilings.MaxConcurrentEvaluations + 1,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeConfig(test.config)
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
			}
		})
	}
}

func TestLowWasmStackLimitReturnsGuestTrap(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{
		MaxFuel:           1_000_000_000,
		MaxStack:          hardCeilings.MaxStack,
		MaxWasmStackBytes: 1 << 20,
	})
	_, err := engine.Evaluate(context.Background(), Request{
		Source: `local f(n) = if n <= 0 then 0 else 1 + f(n - 1); f(1000)`,
	})
	var trap *GuestTrapError
	if !errors.As(err, &trap) {
		t.Fatalf("Evaluate() error = %T: %v, want GuestTrapError", err, err)
	}
}

func TestTraceSurvivesAFailedEvaluation(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		// The message forces the traced value, which laziness would otherwise
		// leave unevaluated.
		Source:       `error "nope: " + std.trace("reached the guard", "detail")`,
		CaptureTrace: true,
	})
	var evaluationErr *EvaluationError
	if !errors.As(err, &evaluationErr) {
		t.Fatalf("expected EvaluationError, got %T: %v", err, err)
	}
	if !strings.Contains(string(result.Trace), "reached the guard") {
		t.Fatalf("trace = %q, want the traced message from before the failure", result.Trace)
	}
	if result.Stats.TraceBytes == 0 {
		t.Fatal("stats must report the captured trace bytes on failure")
	}
	if result.Output != nil || result.Files != nil || result.Documents != nil {
		t.Fatalf("a failed evaluation must not manifest a value: %#v", result)
	}
}

func TestTraceIsAbsentWhenCaptureIsOff(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		Source: `error "nope: " + std.trace("hidden", "detail")`,
	})
	if err == nil {
		t.Fatal("expected the evaluation to fail")
	}
	if len(result.Trace) != 0 {
		t.Fatalf("trace = %q, want nothing without CaptureTrace", result.Trace)
	}
}

func TestStatsReportQueueDuration(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{MaxConcurrentEvaluations: 1})
	release := make(chan struct{})
	blocked := make(chan struct{})
	var wait sync.WaitGroup
	wait.Go(func() {
		_, _ = engine.Evaluate(context.Background(), Request{
			Source: `std.native("hold")()`,
			Capabilities: map[string]Capability{
				"hold": {Call: func(context.Context, []any) (any, error) {
					close(blocked)
					<-release
					return true, nil
				}},
			},
		})
	})
	<-blocked

	// This evaluation cannot start until the slot frees, so it must observe a
	// nonzero queue duration.
	queued := make(chan EvaluationStats, 1)
	go func() {
		result, err := engine.Evaluate(context.Background(), Request{Source: `{a: 1}`})
		if err != nil {
			t.Errorf("queued evaluation: %v", err)
		}
		queued <- result.Stats
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()

	stats := <-queued
	if stats.QueueDuration <= 0 {
		t.Fatalf("QueueDuration = %v, want a positive wait for the concurrency slot", stats.QueueDuration)
	}
}

func TestNewEngineReportsAnAlreadyCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	engine, err := NewEngine(ctx, EngineConfig{})
	if err == nil {
		_ = engine.Close(context.Background())
		t.Fatal("NewEngine() error = nil for a canceled context")
	}
	var canceled *CancellationError
	if !errors.As(err, &canceled) {
		t.Fatalf("expected CancellationError, got %T: %v", err, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation cause to be preserved, got %v", err)
	}
}

func TestPolicySurfaceIsDiscoverable(t *testing.T) {
	defaults := DefaultEngineConfig()
	if defaults != defaultConfig {
		t.Fatalf("DefaultEngineConfig() = %#v, want %#v", defaults, defaultConfig)
	}
	ceilings := Ceilings()
	if ceilings != hardCeilings {
		t.Fatalf("Ceilings() = %#v, want %#v", ceilings, hardCeilings)
	}

	defaults.MaxFuel = 1
	if DefaultEngineConfig().MaxFuel == 1 {
		t.Fatal("DefaultEngineConfig() exposes shared mutable state")
	}
	ceilings.MaxFuel = 1
	if Ceilings().MaxFuel == 1 {
		t.Fatal("Ceilings() exposes shared mutable state")
	}

	if _, err := normalizeConfig(Ceilings()); err != nil {
		t.Fatalf("the reported ceilings must be a valid configuration: %v", err)
	}
}

func TestEngineConfigReportsEffectivePolicy(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{MaxImports: 7})
	effective := engine.Config()
	if effective.MaxImports != 7 {
		t.Fatalf("MaxImports = %d, want 7", effective.MaxImports)
	}
	if effective.MaxFuel != defaultConfig.MaxFuel {
		t.Fatalf("MaxFuel = %d, want the default %d", effective.MaxFuel, defaultConfig.MaxFuel)
	}
}

func TestNormalizeResolvesAndValidatesWithoutAnEngine(t *testing.T) {
	effective, err := EngineConfig{MaxImports: 9}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if effective.MaxImports != 9 {
		t.Fatalf("MaxImports = %d, want 9", effective.MaxImports)
	}
	if effective.MaxFuel != defaultConfig.MaxFuel {
		t.Fatalf("MaxFuel = %d, want the default %d", effective.MaxFuel, defaultConfig.MaxFuel)
	}

	_, err = EngineConfig{MaxFuel: hardCeilings.MaxFuel + 1}.Normalize()
	var invalid *InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
	}
}

func TestEngineConfigRoundTripsThroughJSON(t *testing.T) {
	original := DefaultEngineConfig()
	original.MaxImports = 11
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"max_imports":11`) {
		t.Fatalf("encoded policy is missing a stable field name: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"max_wasm_stack_bytes":50000000`) {
		t.Fatalf("encoded policy is missing max_wasm_stack_bytes: %s", encoded)
	}
	var decoded EngineConfig
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != original {
		t.Fatalf("round-trip = %#v, want %#v", decoded, original)
	}
}

func TestRequestLimitsInheritAndRejectEngineOverflow(t *testing.T) {
	config, err := normalizeConfig(EngineConfig{
		MaxFuel:              50_000_000,
		MaxSourceBytes:       1024,
		MaxOutputBytes:       1024,
		MaxHostResponseBytes: 1024,
		MaxTraceBytes:        1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, normalized, err := prepareRequest(Request{
		Source: `{}`,
		Limits: RequestLimits{
			MaxFuel:        25_000_000,
			MaxOutputBytes: 128,
			MaxTraceBytes:  64,
		},
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Limits.MaxFuel != 25_000_000 ||
		normalized.Limits.MaxOutputBytes != 128 ||
		normalized.Limits.MaxTraceBytes != 64 ||
		normalized.Limits.MaxSourceBytes != 1024 {
		t.Fatalf("unexpected normalized limits: %#v", normalized.Limits)
	}

	_, _, err = prepareRequest(Request{
		Source: `{}`,
		Limits: RequestLimits{MaxFuel: 50_000_001},
	}, config)
	var invalid *InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected engine ceiling rejection, got %T: %v", err, err)
	}
}

func TestPerRequestOutputLimit(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{MaxOutputBytes: 1024})
	_, err := engine.Evaluate(context.Background(), Request{
		Source: `"this output exceeds the request budget"`,
		Limits: RequestLimits{MaxOutputBytes: 8},
	})
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Resource != "output" || limit.Limit != 8 {
		t.Fatalf("expected request output limit, got %T: %v", err, err)
	}
}

func TestTraceCaptureAndStatistics(t *testing.T) {
	importer, err := NewMapImporter(map[string][]byte{
		"value.jsonnet": []byte(`21`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{MaxTraceBytes: 1024})
	result, err := engine.Evaluate(context.Background(), Request{
		Source: `std.trace(std.repeat("x", 256),
			std.native("double")(import "value.jsonnet"))`,
		Importer: importer,
		Capabilities: map[string]Capability{
			"double": {
				Params: []string{"value"},
				Call: func(_ context.Context, args []any) (any, error) {
					return args[0].(float64) * 2, nil
				},
			},
		},
		CaptureTrace: true,
		Limits:       RequestLimits{MaxTraceBytes: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, float64(42))
	if len(result.Trace) != 64 || !result.Stats.TraceTruncated {
		t.Fatalf(
			"trace bytes=%d truncated=%v",
			len(result.Trace),
			result.Stats.TraceTruncated,
		)
	}
	if result.Stats.TraceBytes != uint32(len(result.Trace)) ||
		result.Stats.ImportResolutions != 1 ||
		result.Stats.ImportBytes != 2 ||
		result.Stats.CapabilityCalls != 1 ||
		result.Stats.FuelConsumed == 0 ||
		result.Stats.ExecutionDuration <= 0 {
		t.Fatalf("unexpected evaluation stats: %#v", result.Stats)
	}
}

func TestFuelLimitIsDeterministicAndPerRequest(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	request := Request{Source: `{answer: 6 * 7}`}

	result, err := engine.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	consumed := result.Stats.FuelConsumed
	if consumed == 0 {
		t.Fatal("successful evaluation consumed no fuel")
	}

	request.Limits.MaxFuel = consumed
	result, err = engine.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("exact fuel budget failed: %v", err)
	}
	if result.Stats.FuelConsumed != consumed {
		t.Fatalf("fuel consumed = %d, want deterministic %d", result.Stats.FuelConsumed, consumed)
	}

	request.Limits.MaxFuel = consumed - 1
	_, err = engine.Evaluate(context.Background(), request)
	var limit *LimitError
	if !errors.As(err, &limit) ||
		limit.Resource != "fuel" ||
		limit.Limit != consumed-1 ||
		limit.Actual != consumed {
		t.Fatalf("expected deterministic fuel limit, got %T: %v", err, err)
	}
}

func TestConcurrencyLimitHonorsContext(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{MaxConcurrentEvaluations: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.Evaluate(context.Background(), Request{
			Source: `std.native("hold")()`,
			Capabilities: map[string]Capability{
				"hold": {
					Call: func(context.Context, []any) (any, error) {
						close(started)
						<-release
						return true, nil
					},
				},
			},
		})
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := engine.Evaluate(ctx, Request{Source: `{}`})
	var canceled *CancellationError
	if !errors.As(err, &canceled) {
		t.Fatalf("expected queued cancellation, got %T: %v", err, err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestInvalidUTF8Input(t *testing.T) {
	config, err := normalizeConfig(EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	invalid := string([]byte{0xff})
	for _, request := range []Request{
		{Source: invalid},
		{Source: `{}`, ExtVars: map[string]string{invalid: "value"}},
		{Source: `{}`, ExtCode: map[string]string{"value": invalid}},
		{Source: `{}`, TLAVars: map[string]string{"value": invalid}},
		{Source: `{}`, TLACode: map[string]string{"value": invalid}},
		{
			Source: `{}`,
			Capabilities: map[string]Capability{
				invalid: {Call: func(context.Context, []any) (any, error) { return nil, nil }},
			},
		},
	} {
		if _, _, err := prepareRequest(request, config); err == nil {
			t.Fatalf("expected invalid UTF-8 request to fail: %#v", request)
		}
	}
}

func TestVirtualImportsAndDenials(t *testing.T) {
	importer, err := NewMapImporter(map[string][]byte{
		"lib/value.jsonnet": []byte(`{value: 42}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{
		Filename: "lib/main.jsonnet",
		Source:   `import "./value.jsonnet"`,
		Importer: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"value": float64(42)})

	for _, source := range []string{
		`import "/etc/passwd"`,
		`import "../secret.jsonnet"`,
		`import "a/../secret.jsonnet"`,
		`import "a\\secret.jsonnet"`,
	} {
		_, err := engine.Evaluate(context.Background(), Request{Source: source, Importer: importer})
		var denied *ImportDeniedError
		if !errors.As(err, &denied) {
			t.Errorf("%s: expected ImportDeniedError, got %T: %v", source, err, err)
		}
	}

	_, err = engine.Evaluate(context.Background(), Request{
		Source:   `import "missing.jsonnet"`,
		Importer: importer,
	})
	var denied *ImportDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("expected missing import denial, got %T: %v", err, err)
	}

	_, err = engine.Evaluate(context.Background(), Request{
		Source: `import "panic.jsonnet"`,
		Importer: importerFunc(func(context.Context, string, string) (string, []byte, error) {
			panic("boom")
		}),
	})
	var importErr *ImportError
	if !errors.As(err, &importErr) {
		t.Fatalf("expected importer panic as ImportError, got %T: %v", err, err)
	}
}

func TestEvaluateFileAndRootRelativeImports(t *testing.T) {
	importer, err := NewMapImporter(map[string][]byte{
		"apps/main.jsonnet":  []byte(`import "./value.jsonnet"`),
		"apps/value.jsonnet": []byte(`{value: 42}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.EvaluateFile(context.Background(), "apps/main.jsonnet", Request{
		Importer: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"value": float64(42)})

	result, err = engine.Evaluate(context.Background(), Request{
		Filename: "apps/inline.jsonnet",
		Source:   `import "./value.jsonnet"`,
		Importer: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"value": float64(42)})

	_, err = engine.EvaluateFile(context.Background(), "apps/main.jsonnet", Request{
		Source:   `{}`,
		Importer: importer,
	})
	var invalid *InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected inline file source to be rejected, got %T: %v", err, err)
	}
}

func TestEvaluateAnonymousUsesImporterRoot(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	var importedFrom string
	result, err := engine.EvaluateAnonymous(context.Background(), Request{
		Filename: "diagnostics/main.jsonnet",
		Source:   `import "value.jsonnet"`,
		Importer: importerFunc(func(
			_ context.Context,
			from string,
			importedPath string,
		) (string, []byte, error) {
			importedFrom = from
			if importedPath != "value.jsonnet" {
				return "", nil, ErrImportDenied
			}
			return "value.jsonnet", []byte(`42`), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, float64(42))
	if importedFrom != "" {
		t.Fatalf("anonymous import came from %q", importedFrom)
	}
}

func TestResultReportsResolvedImports(t *testing.T) {
	importer, err := NewMapImporter(map[string][]byte{
		"apps/main.jsonnet": []byte(`{
  first: import "shared.libsonnet",
  again: import "shared.libsonnet",
  nested: import "../lib/nested.libsonnet",
}`),
		"apps/shared.libsonnet":  []byte(`{name: "shared"}`),
		"lib/nested.libsonnet":   []byte(`import "leaf.libsonnet"`),
		"lib/leaf.libsonnet":     []byte(`{leaf: true}`),
		"apps/unused.libsonnet":  []byte(`"never forced"`),
		"apps/failing.jsonnet":   []byte(`(import "shared.libsonnet") + error "stop"`),
		"apps/untouched.jsonnet": []byte(`local unused = import "unused.libsonnet"; 1`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{})

	result, err := engine.EvaluateFile(context.Background(), "apps/main.jsonnet", Request{
		Importer: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The entry point comes first, repeats collapse, and the order is the order
	// the evaluation resolved them in.
	want := []string{
		"apps/main.jsonnet",
		"apps/shared.libsonnet",
		"lib/nested.libsonnet",
		"lib/leaf.libsonnet",
	}
	if !slices.Equal(result.Imports, want) {
		t.Fatalf("Imports = %q, want %q", result.Imports, want)
	}
	if uint32(len(result.Imports)) > result.Stats.ImportResolutions {
		t.Fatalf(
			"Imports has %d entries but only %d resolutions were counted",
			len(result.Imports), result.Stats.ImportResolutions,
		)
	}

	// Laziness decides what is a dependency: an unforced import is absent.
	result, err = engine.EvaluateFile(context.Background(), "apps/untouched.jsonnet", Request{
		Importer: importer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Imports, []string{"apps/untouched.jsonnet"}) {
		t.Fatalf("Imports = %q, want only the entry point", result.Imports)
	}

	// A failed evaluation keeps the evidence of what it had already resolved.
	result, err = engine.EvaluateFile(context.Background(), "apps/failing.jsonnet", Request{
		Importer: importer,
	})
	if err == nil {
		t.Fatal("expected the failing program to report an error")
	}
	if !slices.Contains(result.Imports, "apps/shared.libsonnet") {
		t.Fatalf("Imports = %q, want the import resolved before the failure", result.Imports)
	}

	result, err = engine.Evaluate(context.Background(), Request{Source: `1`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imports != nil {
		t.Fatalf("Imports = %q, want nil without any import", result.Imports)
	}
}

func TestImportLimits(t *testing.T) {
	importer, err := NewMapImporter(map[string][]byte{
		"a.jsonnet": []byte(`"aaaaaaaaaaaaaaaa"`),
		"b.jsonnet": []byte(`"bbbbbbbbbbbbbbbb"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{
		MaxImportBytes:      8,
		MaxTotalImportBytes: 24,
		MaxImports:          1,
	})
	_, err = engine.Evaluate(context.Background(), Request{
		Source:   `import "a.jsonnet"`,
		Importer: importer,
	})
	var limit *LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("expected individual import limit, got %T: %v", err, err)
	}

	engine = newTestEngine(t, EngineConfig{
		MaxImportBytes:      64,
		MaxTotalImportBytes: 64,
		MaxImports:          1,
	})
	_, err = engine.Evaluate(context.Background(), Request{
		Source:   `[import "a.jsonnet", import "b.jsonnet"]`,
		Importer: importer,
	})
	if !errors.As(err, &limit) || limit.Resource != "import count" {
		t.Fatalf("expected import count limit, got %T: %v", err, err)
	}

	engine = newTestEngine(t, EngineConfig{
		MaxImportBytes:      64,
		MaxTotalImportBytes: 20,
		MaxImports:          2,
	})
	_, err = engine.Evaluate(context.Background(), Request{
		Source:   `[import "a.jsonnet", import "b.jsonnet"]`,
		Importer: importer,
	})
	if !errors.As(err, &limit) || limit.Resource != "total import bytes" {
		t.Fatalf("expected cumulative import limit, got %T: %v", err, err)
	}
}

func TestCapabilities(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	var calls atomic.Uint32
	capability := Capability{
		Params: []string{"value"},
		Call: func(_ context.Context, args []any) (any, error) {
			calls.Add(1)
			return args[0].(float64) * 2, nil
		},
	}
	result, err := engine.Evaluate(context.Background(), Request{
		Source:       `local unused = std.native("double")(1); {used: std.native("double")(21)}`,
		Capabilities: map[string]Capability{"double": capability},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"used": float64(42)})
	if calls.Load() != 1 {
		t.Fatalf("capability calls = %d, want 1; unused binding must stay lazy", calls.Load())
	}

	result, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("identity")({nested: [1, true, null]})`,
		Capabilities: map[string]Capability{
			"identity": {
				Params: []string{"value"},
				Call: func(_ context.Context, args []any) (any, error) {
					return args[0], nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{
		"nested": []any{float64(1), true, nil},
	})

	cause := errors.New("handler failed")
	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("fail")()`,
		Capabilities: map[string]Capability{
			"fail": {Call: func(context.Context, []any) (any, error) { return nil, cause }},
		},
	})
	var capabilityErr *CapabilityError
	if !errors.As(err, &capabilityErr) || !errors.Is(err, cause) {
		t.Fatalf("expected wrapped CapabilityError, got %T: %v", err, err)
	}

	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("panic")()`,
		Capabilities: map[string]Capability{
			"panic": {Call: func(context.Context, []any) (any, error) { panic("boom") }},
		},
	})
	if !errors.As(err, &capabilityErr) {
		t.Fatalf("expected panic as CapabilityError, got %T: %v", err, err)
	}

	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("missing")()`,
	})
	var evaluationErr *EvaluationError
	if !errors.As(err, &evaluationErr) {
		t.Fatalf("expected missing capability to be a Jsonnet error, got %T: %v", err, err)
	}
}

func TestCapabilityLimitsAndCancellation(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{
		MaxCapabilityCalls:   1,
		MaxHostResponseBytes: minHostResponseSize,
	})
	capability := Capability{
		Call: func(context.Context, []any) (any, error) { return "ok", nil },
	}
	_, err := engine.Evaluate(context.Background(), Request{
		Source:       `[std.native("query")(), std.native("query")()]`,
		Capabilities: map[string]Capability{"query": capability},
	})
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Resource != "capability calls" {
		t.Fatalf("expected capability call limit, got %T: %v", err, err)
	}

	engine = newTestEngine(t, EngineConfig{MaxHostResponseBytes: minHostResponseSize})
	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("large")()`,
		Capabilities: map[string]Capability{
			"large": {Call: func(context.Context, []any) (any, error) {
				return string(make([]byte, 1024)), nil
			}},
		},
	})
	if !errors.As(err, &limit) || limit.Resource != "capability response bytes" {
		t.Fatalf("expected host response limit, got %T: %v", err, err)
	}

	engine = newTestEngine(t, EngineConfig{MaxHostRequestBytes: 1024})
	var called atomic.Bool
	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("large")(std.repeat("x", 2048))`,
		Capabilities: map[string]Capability{
			"large": {
				Params: []string{"value"},
				Call: func(context.Context, []any) (any, error) {
					called.Store(true)
					return nil, nil
				},
			},
		},
	})
	if !errors.As(err, &limit) ||
		limit.Resource != "host request bytes" ||
		limit.Limit != 1024 ||
		limit.Actual <= limit.Limit {
		t.Fatalf("expected guest host-request limit, got %T: %v", err, err)
	}
	if called.Load() {
		t.Fatal("oversized guest request reached capability handler")
	}

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	_, err = engine.Evaluate(ctx, Request{
		Source: `std.native("cancel")()`,
		Capabilities: map[string]Capability{
			"cancel": {Call: func(ctx context.Context, _ []any) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}},
		},
	})
	var canceled *CancellationError
	if !errors.As(err, &canceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected CancellationError, got %T: %v", err, err)
	}
}

func TestSourceAndOutputLimits(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{MaxSourceBytes: 8, MaxOutputBytes: 16})
	_, err := engine.Evaluate(context.Background(), Request{Source: `"source is too long"`})
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Resource != "source bytes" {
		t.Fatalf("expected source limit, got %T: %v", err, err)
	}

	_, err = engine.Evaluate(context.Background(), Request{Source: `"this output is too long"`})
	if !errors.As(err, &limit) || limit.Resource != "source bytes" {
		t.Fatalf("expected source to remain the first enforced limit, got %T: %v", err, err)
	}

	engine = newTestEngine(t, EngineConfig{MaxSourceBytes: 64, MaxOutputBytes: 8})
	_, err = engine.Evaluate(context.Background(), Request{Source: `"this output is too long"`})
	if !errors.As(err, &limit) ||
		limit.Resource != "output" ||
		limit.Limit != 8 ||
		limit.Actual <= limit.Limit {
		t.Fatalf("expected output limit, got %T: %v", err, err)
	}
}

func TestCancellationDuringEvaluation(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := engine.Evaluate(ctx, Request{
		Source: `std.foldl(function(acc, x) acc + x, std.range(1, 100000000), 0)`,
	})
	var canceled *CancellationError
	if !errors.As(err, &canceled) {
		t.Fatalf("expected CancellationError, got %T: %v", err, err)
	}
}

func TestWASMMemoryExhaustion(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{
		MaxMemoryBytes: 32 << 20,
		MaxFuel:        hardCeilings.MaxFuel,
		MaxOutputBytes: 64 << 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := engine.Evaluate(ctx, Request{
		Source: `std.makeArray(2000000, function(x) {index: x, value: "abcdefgh"})`,
	})
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Resource != "WASM memory" {
		t.Fatalf("expected WASM memory limit, got %T: %v", err, err)
	}
}

func TestFreshInstancesAndConcurrentEvaluation(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	for i := range 2 {
		result, err := engine.Evaluate(context.Background(), Request{
			Source:  `std.extVar("value")`,
			ExtVars: map[string]string{"value": fmt.Sprint(i)},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertJSON(t, result.Output, fmt.Sprint(i))
	}

	const count = 16
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			result, err := engine.Evaluate(context.Background(), Request{
				Source: fmt.Sprintf(`{value: %d}`, value),
			})
			if err == nil {
				var decoded map[string]int
				err = json.Unmarshal(result.Output, &decoded)
				if err == nil && decoded["value"] != value {
					err = fmt.Errorf("got %d, want %d", decoded["value"], value)
				}
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestClose(t *testing.T) {
	engine := newTestEngine(t, EngineConfig{})
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Evaluate(context.Background(), Request{Source: `{}`})
	var closed *EngineClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("expected EngineClosedError, got %T: %v", err, err)
	}
}

func TestABIVersionMismatch(t *testing.T) {
	version := Version()
	if version.Jsonnet != jsonnet.Version() || version.ABI != protocol.ABIVersion {
		t.Fatalf("unexpected version info: %#v", version)
	}
	if err := validateABIVersion(protocol.ABIVersion); err != nil {
		t.Fatalf("current ABI version was rejected: %v", err)
	}
	err := validateABIVersion(protocol.ABIVersion - 1)
	var abiErr *ABIError
	if !errors.As(err, &abiErr) {
		t.Fatalf("expected ABIError, got %T: %v", err, err)
	}
}

func TestGoldenAgainstNativeGoJsonnet(t *testing.T) {
	const source = `local f(x) = x * x; {squares: [f(x) for x in std.range(1, 5)]}`
	native, err := jsonnet.MakeVM().EvaluateAnonymousSnippet("snippet.jsonnet", source)
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, EngineConfig{})
	result, err := engine.Evaluate(context.Background(), Request{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != native {
		t.Fatalf("guest output differs from native\nwant: %s\ngot: %s", native, result.Output)
	}
}

func TestMalformedHostPointerAndLength(t *testing.T) {
	mod := wazerotest.NewModule(wazerotest.NewMemory(64))
	if _, err := readHostRequest(mod, 65534, 8, 16); err == nil {
		t.Fatal("expected out-of-bounds request error")
	}
	if _, err := readHostRequest(mod, 0, 17, 16); err == nil {
		t.Fatal("expected request size error")
	}
	if _, err := readHostRequest(mod, 0, 8, 16); err == nil {
		t.Fatal("expected zero request pointer error")
	}
	if _, err := hostResponseBuffer(mod, 65535, 8, 16); err == nil {
		t.Fatal("expected out-of-bounds response error")
	}
	if _, err := hostResponseBuffer(mod, 1, 17, 16); err == nil {
		t.Fatal("expected response capacity error")
	}
	if _, err := hostResponseBuffer(mod, 0, 8, 16); err == nil {
		t.Fatal("expected zero response pointer error")
	}
	if _, err := readGuestResult(mod, 65535, 8, 16); err == nil {
		t.Fatal("expected malformed guest result pointer")
	}
	if _, err := readGuestResult(mod, 0, 17, 16); err == nil {
		t.Fatal("expected oversized guest result")
	}
	if _, err := readGuestResult(mod, 1, 0, 16); err == nil {
		t.Fatal("expected noncanonical empty result")
	}

	response := make([]byte, 5)
	packed := writeHostResponse(response, protocol.HostHandlerFailure, []byte("ééé"))
	status, length := protocol.Unpack(packed)
	if status != protocol.HostHandlerFailure || length != 4 || !json.Valid([]byte(`"`+string(response[:length])+`"`)) {
		t.Fatalf("unexpected truncated UTF-8 response: %d %d %q", status, length, response[:length])
	}
	if packed := writeHostResponse(make([]byte, 2), protocol.HostOK, []byte("large")); packed != protocol.Pack(protocol.HostLimit, 0) {
		t.Fatalf("unexpected oversized success result: %x", packed)
	}
}

func TestGenericHostCallDispatchAndValidation(t *testing.T) {
	mod := wazerotest.NewModule(wazerotest.NewMemory(64))
	request := []byte(`{"name":"echo","args":[{"nested":[1,true,null]}]}`)
	const requestPtr = uint32(64)
	const responsePtr = uint32(1024)
	const responseCap = uint32(256)
	if !mod.Memory().Write(requestPtr, request) {
		t.Fatal("write request")
	}

	state := &invocationState{
		limits: RequestLimits{
			MaxHostRequestBytes:  responseCap,
			MaxHostResponseBytes: responseCap,
			MaxCapabilityCalls:   1,
		},
		request: Request{
			Capabilities: map[string]Capability{
				"echo": {
					Params: []string{"value"},
					Call: func(_ context.Context, args []any) (any, error) {
						return args[0], nil
					},
				},
			},
		},
	}
	ctx := context.WithValue(context.Background(), invocationKey{}, state)
	engine := &Engine{}
	packed := engine.hostCall(
		ctx,
		mod,
		protocol.OperationCallCapability,
		requestPtr,
		uint32(len(request)),
		responsePtr,
		responseCap,
	)
	status, length := protocol.Unpack(packed)
	if status != protocol.HostOK {
		t.Fatalf("unexpected host status %d", status)
	}
	response, ok := mod.Memory().Read(responsePtr, length)
	if !ok {
		t.Fatal("read response")
	}
	var decoded protocol.CapabilityResponse
	if err := protocol.DecodeJSON(response, &decoded); err != nil {
		t.Fatal(err)
	}

	badState := &invocationState{limits: state.limits}
	badCtx := context.WithValue(context.Background(), invocationKey{}, badState)
	packed = engine.hostCall(
		badCtx,
		mod,
		99,
		requestPtr,
		uint32(len(request)),
		responsePtr,
		responseCap,
	)
	status, _ = protocol.Unpack(packed)
	var abiErr *ABIError
	if status != protocol.HostMalformed || !errors.As(badState.error(), &abiErr) {
		t.Fatalf("expected malformed operation ABI error, got %d %v", status, badState.error())
	}

	packed = engine.hostCall(
		badCtx,
		mod,
		protocol.OperationCallCapability,
		65535,
		8,
		responsePtr,
		responseCap,
	)
	status, _ = protocol.Unpack(packed)
	if status != protocol.HostMalformed {
		t.Fatalf("expected malformed pointer status, got %d", status)
	}

	errorRequest := []byte(`{"name":"fail","args":[]}`)
	if !mod.Memory().Write(requestPtr, errorRequest) {
		t.Fatal("write error request")
	}
	errorState := &invocationState{
		limits: state.limits,
		request: Request{
			Capabilities: map[string]Capability{
				"fail": {
					Call: func(context.Context, []any) (any, error) {
						return nil, errors.New(strings.Repeat("é", int(responseCap)))
					},
				},
			},
		},
	}
	errorCtx := context.WithValue(context.Background(), invocationKey{}, errorState)
	packed = engine.hostCall(
		errorCtx,
		mod,
		protocol.OperationCallCapability,
		requestPtr,
		uint32(len(errorRequest)),
		responsePtr,
		responseCap,
	)
	status, length = protocol.Unpack(packed)
	var capabilityErr *CapabilityError
	if status != protocol.HostHandlerFailure ||
		length > responseCap ||
		!errors.As(errorState.error(), &capabilityErr) {
		t.Fatalf(
			"oversized handler error changed status: status=%d length=%d error=%v",
			status,
			length,
			errorState.error(),
		)
	}
}

func TestCapabilityArityMismatchIsRejected(t *testing.T) {
	mod := wazerotest.NewModule(wazerotest.NewMemory(64))
	// A well-behaved guest builds the signature from the declared parameters,
	// so this request could only come from a broken or hostile guest.
	request := []byte(`{"name":"pair","args":[1]}`)
	const requestPtr = uint32(64)
	const responsePtr = uint32(1024)
	const responseCap = uint32(256)
	if !mod.Memory().Write(requestPtr, request) {
		t.Fatal("write request")
	}

	var called bool
	state := &invocationState{
		limits: RequestLimits{
			MaxHostRequestBytes:  responseCap,
			MaxHostResponseBytes: responseCap,
			MaxCapabilityCalls:   1,
		},
		request: Request{
			Capabilities: map[string]Capability{
				"pair": {
					Params: []string{"first", "second"},
					Call: func(_ context.Context, args []any) (any, error) {
						called = true
						return args[1], nil
					},
				},
			},
		},
	}
	ctx := context.WithValue(context.Background(), invocationKey{}, state)
	engine := &Engine{}
	packed := engine.hostCall(
		ctx,
		mod,
		protocol.OperationCallCapability,
		requestPtr,
		uint32(len(request)),
		responsePtr,
		responseCap,
	)
	status, _ := protocol.Unpack(packed)
	if status != protocol.HostMalformed {
		t.Fatalf("status = %d, want HostMalformed for an arity mismatch", status)
	}
	if called {
		t.Fatal("the handler must not run with fewer arguments than it declared")
	}
	var capabilityErr *CapabilityError
	if !errors.As(state.error(), &capabilityErr) {
		t.Fatalf("expected CapabilityError, got %T: %v", state.error(), state.error())
	}
}

func FuzzDecodeHostPayloads(f *testing.F) {
	f.Add([]byte(`{"from":"","path":"x.jsonnet"}`))
	f.Add([]byte(`{"name":"query","args":[1,true,null]}`))
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, payload []byte) {
		var importRequest protocol.ImportRequest
		_ = protocol.DecodeJSON(payload, &importRequest)
		var capabilityRequest protocol.CapabilityRequest
		_ = protocol.DecodeJSON(payload, &capabilityRequest)
		var evaluationRequest protocol.EvaluationRequest
		_ = protocol.DecodeJSON(payload, &evaluationRequest)
	})
}

func newTestEngine(t *testing.T, config EngineConfig) *Engine {
	t.Helper()
	engine, err := newEngineForTest(context.Background(), config)
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

func assertJSON(t *testing.T, actual []byte, expected any) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(actual, &decoded); err != nil {
		t.Fatalf("decode output: %v\n%s", err, actual)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(expectedJSON, &normalized); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%#v", decoded) != fmt.Sprintf("%#v", normalized) {
		t.Fatalf("JSON mismatch\nwant: %#v\ngot:  %#v", normalized, decoded)
	}
}

type importerFunc func(context.Context, string, string) (string, []byte, error)

func (f importerFunc) Import(
	ctx context.Context,
	importedFrom string,
	importedPath string,
) (string, []byte, error) {
	return f(ctx, importedFrom, importedPath)
}
