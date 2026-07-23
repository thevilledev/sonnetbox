package securejsonnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
			name:   "request ceiling",
			config: EngineConfig{MaxHostRequestBytes: hardCeilings.MaxHostRequestBytes + 1},
		},
		{
			name:   "response minimum",
			config: EngineConfig{MaxHostResponseBytes: minHostResponseSize - 1},
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
		config: EngineConfig{
			MaxHostRequestBytes:  responseCap,
			MaxHostResponseBytes: responseCap,
			MaxCapabilityCalls:   1,
		},
		request: Request{
			Capabilities: map[string]Capability{
				"echo": {
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

	badState := &invocationState{config: state.config}
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
		config: state.config,
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
