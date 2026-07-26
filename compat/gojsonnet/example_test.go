package gojsonnet_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/thevilledev/sonnetbox"
	sonnetcompat "github.com/thevilledev/sonnetbox/compat/gojsonnet"
)

// Example migrates a go-jsonnet VM workflow. The shape of the code is the same;
// the differences are a long-lived engine, an explicit importer, and a context
// on every evaluation.
func Example() {
	engine, err := sonnetbox.NewEngine(
		context.Background(),
		sonnetcompat.RecommendedEngineConfig(),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	imports, err := sonnetbox.NewMapImporter(map[string][]byte{
		"apps/main.jsonnet": []byte(`{
  environment: std.extVar("environment"),
  service: import "../lib/service.libsonnet",
}`),
		"lib/service.libsonnet": []byte(`{name: "checkout", replicas: 3}`),
	})
	if err != nil {
		panic(err)
	}

	vm, err := sonnetcompat.New(engine)
	if err != nil {
		panic(err)
	}
	vm.Importer(imports)
	vm.ExtVar("environment", "production")
	vm.SetTraceOut(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := vm.EvaluateFile(ctx, "apps/main.jsonnet")
	if err != nil {
		panic(err)
	}
	fmt.Print(output)

	// Output:
	// {
	//    "environment": "production",
	//    "service": {
	//       "name": "checkout",
	//       "replicas": 3
	//    }
	// }
}

// ExampleVM_FindDependencies recovers the build-graph information a go-jsonnet
// caller would get from FindDependencies. It reports what the evaluation
// resolved rather than what a static parse could reach.
func ExampleVM_FindDependencies() {
	engine, err := sonnetbox.NewEngine(
		context.Background(),
		sonnetcompat.RecommendedEngineConfig(),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			panic(err)
		}
	}()

	imports, err := sonnetbox.NewMapImporter(map[string][]byte{
		"apps/main.jsonnet":      []byte(`(import "../lib/service.libsonnet") + {tier: "web"}`),
		"lib/service.libsonnet":  []byte(`{defaults: import "defaults.libsonnet"}`),
		"lib/defaults.libsonnet": []byte(`{replicas: 2}`),
	})
	if err != nil {
		panic(err)
	}

	vm, err := sonnetcompat.New(engine)
	if err != nil {
		panic(err)
	}
	vm.Importer(imports)
	vm.SetTraceOut(nil)

	dependencies, err := vm.FindDependencies(
		context.Background(),
		"apps/entry.jsonnet",
		[]string{"main.jsonnet"},
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Join(dependencies, "\n"))

	// Output:
	// lib/defaults.libsonnet
	// lib/service.libsonnet
}
