package sonnetbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCompilationCacheIsReusedAcrossEngines(t *testing.T) {
	cache := NewCompilationCache()
	t.Cleanup(func() {
		if err := cache.Close(context.Background()); err != nil {
			t.Errorf("close cache: %v", err)
		}
	})

	for range 2 {
		engine, err := NewEngine(context.Background(), EngineConfig{}, WithCompilationCache(cache))
		if err != nil {
			t.Fatal(err)
		}
		result, err := engine.Evaluate(context.Background(), Request{Source: `{answer: 6 * 7}`})
		if err != nil {
			t.Fatal(err)
		}
		assertJSON(t, result.Output, map[string]any{"answer": float64(42)})
		if err := engine.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompilationCacheDirPersistsAcrossEngines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "guest-cache")
	cache, err := NewCompilationCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithCompilationCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A second cache over the same directory must find the earlier artifact and
	// still produce a working engine.
	reopened, err := NewCompilationCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(context.Background()); err != nil {
			t.Errorf("close cache: %v", err)
		}
	})
	warm, err := NewEngine(context.Background(), EngineConfig{}, WithCompilationCache(reopened))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := warm.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	result, err := warm.Evaluate(context.Background(), Request{Source: `{answer: 6 * 7}`})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"answer": float64(42)})
}

func TestCompilationCacheCloseIsIdempotent(t *testing.T) {
	cache := NewCompilationCache()
	if err := cache.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	var nilCache *CompilationCache
	if err := nilCache.Close(context.Background()); err != nil {
		t.Fatalf("nil close: %v", err)
	}
	if err := cache.Close(nil); err == nil { //nolint:staticcheck // exercises the nil-context guard.
		t.Fatal("expected a nil context to be rejected")
	}
}

func TestInterpreterOptionEvaluates(t *testing.T) {
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithInterpreter())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	result, err := engine.Evaluate(context.Background(), Request{Source: `{answer: 6 * 7}`})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"answer": float64(42)})
}

func TestDefaultImporterServesRequestsThatSupplyNone(t *testing.T) {
	shared, err := NewMapImporter(map[string][]byte{
		"shared.libsonnet": []byte(`{origin: "engine"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithDefaultImporter(shared))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	result, err := engine.Evaluate(context.Background(), Request{
		Source: `import "shared.libsonnet"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"origin": "engine"})

	// A request importer replaces the default rather than layering onto it.
	override, err := NewMapImporter(map[string][]byte{
		"shared.libsonnet": []byte(`{origin: "request"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = engine.Evaluate(context.Background(), Request{
		Source:   `import "shared.libsonnet"`,
		Importer: override,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"origin": "request"})
}

func TestDefaultCapabilitiesApplyAndYieldToRequests(t *testing.T) {
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithDefaultCapabilities(
		map[string]Capability{
			"origin": {Call: func(context.Context, []any) (any, error) {
				return "engine", nil
			}},
			"constant": {Call: func(context.Context, []any) (any, error) {
				return float64(7), nil
			}},
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	result, err := engine.Evaluate(context.Background(), Request{
		Source: `{origin: std.native("origin")(), constant: std.native("constant")()}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"origin": "engine", "constant": float64(7)})

	// A request may replace a default by name while the others remain.
	result, err = engine.Evaluate(context.Background(), Request{
		Source: `{origin: std.native("origin")(), constant: std.native("constant")()}`,
		Capabilities: map[string]Capability{
			"origin": {Call: func(context.Context, []any) (any, error) {
				return "request", nil
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, map[string]any{"origin": "request", "constant": float64(7)})
}

func TestDefaultCapabilitiesAreValidatedUpFront(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities map[string]Capability
	}{
		{
			name:         "nil call",
			capabilities: map[string]Capability{"broken": {}},
		},
		{
			name:         "empty name",
			capabilities: map[string]Capability{"": {Call: func(context.Context, []any) (any, error) { return nil, nil }}},
		},
		{
			name: "bad parameter",
			capabilities: map[string]Capability{"lookup": {
				Params: []string{"not an identifier"},
				Call:   func(context.Context, []any) (any, error) { return nil, nil },
			}},
		},
		{
			name: "duplicate parameter",
			capabilities: map[string]Capability{"lookup": {
				Params: []string{"key", "key"},
				Call:   func(context.Context, []any) (any, error) { return nil, nil },
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewEngine(
				context.Background(),
				EngineConfig{},
				WithDefaultCapabilities(test.capabilities),
			)
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
			}
		})
	}
}

func TestDefaultCapabilitiesAreCopied(t *testing.T) {
	capabilities := map[string]Capability{
		"origin": {Call: func(context.Context, []any) (any, error) { return "engine", nil }},
	}
	engine, err := NewEngine(context.Background(), EngineConfig{}, WithDefaultCapabilities(capabilities))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	// Mutating the caller's map must not change what the engine grants.
	delete(capabilities, "origin")
	capabilities["injected"] = Capability{
		Call: func(context.Context, []any) (any, error) { return "late", nil },
	}

	result, err := engine.Evaluate(context.Background(), Request{
		Source: `std.native("origin")()`,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, result.Output, "engine")

	if _, err := engine.Evaluate(context.Background(), Request{
		Source: `std.native("injected")()`,
	}); err == nil {
		t.Fatal("a capability added after construction must not be visible")
	}
}

func TestInvalidEngineOptionsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		option Option
	}{
		{name: "nil option", option: nil},
		{name: "nil cache", option: WithCompilationCache(nil)},
		{name: "zero-value cache", option: WithCompilationCache(&CompilationCache{})},
		{name: "nil importer", option: WithDefaultImporter(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewEngine(context.Background(), EngineConfig{}, test.option)
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
			}
		})
	}

	if _, err := NewCompilationCacheDir(""); err == nil {
		t.Fatal("expected an empty cache directory to be rejected")
	}
}
