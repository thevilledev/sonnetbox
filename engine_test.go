package securejsonnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jsonnet "github.com/google/go-jsonnet"
	"github.com/tetratelabs/wazero/experimental/wazerotest"
	"github.com/thevilledev/wasmnet/internal/protocol"
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
		Source:   `import "lib/value.jsonnet"`,
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
	if calls.Load() == 0 {
		t.Fatal("capability was not called")
	}

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
		MaxHostResponseBytes: 32,
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

	engine = newTestEngine(t, EngineConfig{MaxHostResponseBytes: 32})
	_, err = engine.Evaluate(context.Background(), Request{
		Source: `std.native("large")()`,
		Capabilities: map[string]Capability{
			"large": {Call: func(context.Context, []any) (any, error) {
				return string(make([]byte, 128)), nil
			}},
		},
	})
	if !errors.As(err, &limit) || limit.Resource != "host response" {
		t.Fatalf("expected host response limit, got %T: %v", err, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err = engine.Evaluate(ctx, Request{
		Source: `std.native("cancel")()`,
		Capabilities: map[string]Capability{
			"cancel": {Call: func(ctx context.Context, _ []any) (any, error) {
				cancel()
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
	if !errors.As(err, &limit) || limit.Resource != "output" {
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
		MaxOutputBytes: 64 << 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	for i := 0; i < 2; i++ {
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
	for i := 0; i < count; i++ {
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
	engine, err := NewEngine(context.Background(), EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Evaluate(context.Background(), Request{Source: `{}`})
	var closed *EngineClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("expected EngineClosedError, got %T: %v", err, err)
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
	if packed := writeHostResponse(mod, 65535, 8, protocol.HostOK, []byte("payload")); packed != protocol.Pack(protocol.HostMalformed, 0) {
		t.Fatalf("unexpected malformed response result: %x", packed)
	}
	if packed := writeHostResponse(mod, 0, 2, protocol.HostOK, []byte("payload")); packed != protocol.Pack(protocol.HostLimit, 0) {
		t.Fatalf("unexpected oversized response result: %x", packed)
	}
	if _, err := readGuestResult(mod, 65535, 8, 16); err == nil {
		t.Fatal("expected malformed guest result pointer")
	}
	if _, err := readGuestResult(mod, 0, 17, 16); err == nil {
		t.Fatal("expected oversized guest result")
	}
}

func FuzzDecodeHostPayloads(f *testing.F) {
	f.Add([]byte(`{"imported_from":"","imported_path":"x.jsonnet"}`))
	f.Add([]byte(`{"name":"query","args":[1,true,null]}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var importRequest protocol.ImportRequest
		_ = decodeJSON(payload, &importRequest)
		var capabilityRequest protocol.CapabilityRequest
		_ = decodeJSON(payload, &capabilityRequest)
		var evaluationRequest protocol.EvaluationRequest
		_ = decodeJSON(payload, &evaluationRequest)
	})
}

func newTestEngine(t *testing.T, config EngineConfig) *Engine {
	t.Helper()
	engine, err := NewEngine(context.Background(), config)
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
