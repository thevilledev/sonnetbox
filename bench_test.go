package sonnetbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkNewEngineCold measures the cost a process pays when it compiles the
// embedded guest from scratch. This is the number that makes a long-lived
// engine worthwhile.
func BenchmarkNewEngineCold(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		engine, err := NewEngine(ctx, EngineConfig{})
		if err != nil {
			b.Fatal(err)
		}
		if err := engine.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewEngineCached measures engine creation when compiled guest code is
// reused, which is the realistic cost for a short-lived command.
func BenchmarkNewEngineCached(b *testing.B) {
	ctx := context.Background()
	cache, err := NewCompilationCacheDir(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := cache.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	// Pay the compilation cost once so the measured iterations all hit the cache.
	warm, err := NewEngine(ctx, EngineConfig{}, WithCompilationCache(cache))
	if err != nil {
		b.Fatal(err)
	}
	if err := warm.Close(ctx); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		engine, err := NewEngine(ctx, EngineConfig{}, WithCompilationCache(cache))
		if err != nil {
			b.Fatal(err)
		}
		if err := engine.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewEngineInterpreter measures the alternative for a single
// evaluation: a much cheaper start in exchange for slower evaluation.
func BenchmarkNewEngineInterpreter(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		engine, err := NewEngine(ctx, EngineConfig{}, WithInterpreter())
		if err != nil {
			b.Fatal(err)
		}
		if err := engine.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEvaluateSnippet isolates the fixed per-evaluation cost of a fresh
// guest instance and one bounded message exchange.
func BenchmarkEvaluateSnippet(b *testing.B) {
	ctx := context.Background()
	engine := newBenchEngine(b)
	request := Request{Source: `{answer: 6 * 7}`}
	b.ResetTimer()
	for b.Loop() {
		if _, err := engine.Evaluate(ctx, request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEvaluateInterpreterSnippet shows the same evaluation on the
// interpreter, so the startup saving can be weighed against it.
func BenchmarkEvaluateInterpreterSnippet(b *testing.B) {
	ctx := context.Background()
	engine, err := NewEngine(ctx, EngineConfig{}, WithInterpreter())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := engine.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	request := Request{Source: `{answer: 6 * 7}`}
	b.ResetTimer()
	for b.Loop() {
		if _, err := engine.Evaluate(ctx, request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEvaluateImports measures a tree of host import callbacks, which is
// where a sandbox boundary costs more than an in-process VM.
func BenchmarkEvaluateImports(b *testing.B) {
	const depth = 16
	files := make(map[string][]byte, depth+1)
	for level := range depth {
		files[fmt.Sprintf("lib/level%d.libsonnet", level)] = fmt.Appendf(
			nil, `{level: %d, next: import "level%d.libsonnet"}`, level, level+1,
		)
	}
	files[fmt.Sprintf("lib/level%d.libsonnet", depth)] = []byte(`{leaf: true}`)
	importer, err := NewMapImporter(files)
	if err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	engine := newBenchEngine(b)
	request := Request{
		Filename: "lib/main.jsonnet",
		Source:   `import "level0.libsonnet"`,
		Importer: importer,
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := engine.Evaluate(ctx, request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEvaluateLargeManifest measures a result large enough that Jsonnet
// evaluation and JSON rendering dominate the fixed sandbox overhead. A render
// this size needs more than the default instruction budget, which is itself
// worth knowing when sizing a policy.
func BenchmarkEvaluateLargeManifest(b *testing.B) {
	ctx := context.Background()
	engine := newBenchEngineWithConfig(b, EngineConfig{MaxFuel: 3_000_000_000})
	request := Request{
		Source: `{
  services: {
    ["service-" + index]: {
      name: "service-" + index,
      replicas: index % 5 + 1,
      ports: [8000 + index, 9000 + index],
      labels: {tier: if index % 2 == 0 then "web" else "worker"},
    }
    for index in std.range(1, 600)
  },
}`,
	}
	b.ResetTimer()
	var outputBytes int
	for b.Loop() {
		result, err := engine.Evaluate(ctx, request)
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = len(result.Output)
	}
	b.ReportMetric(float64(outputBytes), "output_bytes")
}

// BenchmarkEvaluateCapability measures one guest-to-host native call round
// trip, the other place the boundary is visible.
func BenchmarkEvaluateCapability(b *testing.B) {
	ctx := context.Background()
	engine := newBenchEngine(b)
	request := Request{
		Source: `std.length([std.native("lookup")("key-%d" % i) for i in std.range(1, 32)])`,
		Capabilities: map[string]Capability{
			"lookup": {
				Params: []string{"key"},
				Call: func(_ context.Context, args []any) (any, error) {
					key, ok := args[0].(string)
					if !ok {
						return nil, fmt.Errorf("key is %T, want string", args[0])
					}
					return strings.ToUpper(key), nil
				},
			},
		},
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := engine.Evaluate(ctx, request); err != nil {
			b.Fatal(err)
		}
	}
}

func newBenchEngine(b *testing.B) *Engine {
	b.Helper()
	return newBenchEngineWithConfig(b, EngineConfig{})
}

func newBenchEngineWithConfig(b *testing.B, config EngineConfig) *Engine {
	b.Helper()
	ctx := context.Background()
	engine, err := NewEngine(ctx, config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := engine.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	return engine
}
