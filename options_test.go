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

func TestInvalidEngineOptionsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		option Option
	}{
		{name: "nil option", option: nil},
		{name: "nil cache", option: WithCompilationCache(nil)},
		{name: "zero-value cache", option: WithCompilationCache(&CompilationCache{})},
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
