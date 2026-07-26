package sonnetbox_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/thevilledev/sonnetbox"
)

func Example() {
	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	result, err := engine.Evaluate(ctx, sonnetbox.Request{
		Filename: "main.jsonnet",
		Source:   `{answer: 6 * 7}`,
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(result.Output))

	// Output:
	// {
	//    "answer": 42
	// }
}

// ExampleNewWorkspaceImporter grants a workspace root plus one library
// directory that lives outside it, which is how a go-jsonnet
// FileImporter with absolute or parent-directory JPaths is expressed.
func ExampleNewWorkspaceImporter() {
	root, shared, cleanup := exampleWorkspace()
	defer cleanup()

	workspace, err := sonnetbox.NewWorkspaceImporter(
		root,
		sonnetbox.WithLibraryPaths("vendor"),
		sonnetbox.WithSearchRoot("shared", shared),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := workspace.Close(); err != nil {
			panic(err)
		}
	}()

	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	result, err := engine.EvaluateFile(ctx, "apps/main.jsonnet", sonnetbox.Request{
		Importer: workspace,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(result.Output))
	// Imports reports what the evaluation resolved, including the path a search
	// root served, which is what dependency tracking needs.
	fmt.Println(strings.Join(result.Imports, "\n"))

	// Output:
	// {
	//    "limits": {
	//       "memory": "512Mi"
	//    },
	//    "region": "eu-north-1"
	// }
	//
	// apps/main.jsonnet
	// vendor/limits.libsonnet
	// shared/region.libsonnet
}

// ExampleCapability exposes one pure host lookup as a Jsonnet native function.
func ExampleCapability() {
	regions := map[string]string{"prod": "eu-north-1", "dev": "eu-west-1"}

	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	result, err := engine.Evaluate(ctx, sonnetbox.Request{
		Source: `{region: std.native("region")("prod")}`,
		Capabilities: map[string]sonnetbox.Capability{
			"region": {
				Params: []string{"environment"},
				Call: func(_ context.Context, args []any) (any, error) {
					environment, ok := args[0].(string)
					if !ok {
						return nil, fmt.Errorf("environment is %T, want string", args[0])
					}
					region, known := regions[environment]
					if !known {
						return nil, fmt.Errorf("unknown environment %q", environment)
					}
					return region, nil
				},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(result.Output))

	// Output:
	// {
	//    "region": "eu-north-1"
	// }
}

// ExampleWithObserver records every import attempt, including the denials a
// program probing for ungranted files would otherwise make invisibly.
func ExampleWithObserver() {
	imports, err := sonnetbox.NewMapImporter(map[string][]byte{
		"granted.libsonnet": []byte(`{granted: true}`),
	})
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{}, sonnetbox.WithObserver(
		&sonnetbox.Observer{
			Import: func(_ context.Context, event sonnetbox.ImportEvent) {
				outcome := "served"
				if event.Denied {
					outcome = "denied"
				}
				fmt.Printf("%s %s\n", outcome, event.ImportedPath)
			},
		},
	))
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	_, err = engine.Evaluate(ctx, sonnetbox.Request{
		Source: `{
  ok: import "granted.libsonnet",
  probe: import "ungranted.libsonnet",
}`,
		Importer: imports,
	})
	if err == nil {
		panic("expected the ungranted import to be denied")
	}

	// Output:
	// served granted.libsonnet
	// denied ungranted.libsonnet
}

// ExampleNewSlogObserver routes the same activity into structured logs, with
// denials at warn level because those are the security-relevant events.
func ExampleNewSlogObserver() {
	// Timings and fuel vary between runs, so drop them to keep this output
	// comparable. A real deployment keeps them.
	varying := map[string]bool{
		slog.TimeKey:         true,
		"duration":           true,
		"queue_duration":     true,
		"execution_duration": true,
		"fuel_consumed":      true,
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if varying[attr.Key] {
				return slog.Attr{}
			}
			return attr
		},
	}))

	ctx := context.Background()
	engine, err := sonnetbox.NewEngine(
		ctx,
		sonnetbox.EngineConfig{},
		sonnetbox.WithObserver(sonnetbox.NewSlogObserver(logger)),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	// A nil importer denies every import.
	if _, err := engine.Evaluate(ctx, sonnetbox.Request{
		Source: `import "secrets.libsonnet"`,
	}); err == nil {
		panic("expected the import to be denied")
	}

	// Output:
	// level=WARN msg="sonnetbox import denied" imported_path=secrets.libsonnet imported_from=snippet.jsonnet error="import \"secrets.libsonnet\" from \"snippet.jsonnet\" denied: import denied"
	// level=ERROR msg="sonnetbox evaluation failed" filename=snippet.jsonnet import_resolutions=1 import_bytes=0 capability_calls=0 error="import \"secrets.libsonnet\" from \"snippet.jsonnet\" denied: import denied"
}

// exampleWorkspace lays out a workspace root and a separate shared library
// directory outside it.
func exampleWorkspace() (root string, shared string, cleanup func()) {
	parent, err := os.MkdirTemp("", "sonnetbox-example")
	if err != nil {
		panic(err)
	}
	root = filepath.Join(parent, "workspace")
	shared = filepath.Join(parent, "shared")
	files := map[string]string{
		filepath.Join(root, "apps", "main.jsonnet"): `{
  limits: import "limits.libsonnet",
  region: (import "region.libsonnet").region,
}`,
		filepath.Join(root, "vendor", "limits.libsonnet"): `{memory: "512Mi"}`,
		filepath.Join(shared, "region.libsonnet"):         `{region: "eu-north-1"}`,
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			panic(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			panic(err)
		}
	}
	return root, shared, func() {
		if err := os.RemoveAll(parent); err != nil {
			panic(err)
		}
	}
}
