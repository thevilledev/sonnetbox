# securejsonnet

`securejsonnet` evaluates untrusted Jsonnet in a fresh WebAssembly sandbox. It
uses the pure-Go
[go-jsonnet](https://github.com/google/go-jsonnet) evaluator inside a Go WASI
reactor and the pure-Go [wazero](https://wazero.io/) runtime in the host. It
does not use Cgo, C++, Wasmtime, shared libraries, or go-jsonnet's browser-only
`js/wasm` artifact.

The module requires Go 1.24.5 or newer.

## Example

```go
ctx := context.Background()
engine, err := securejsonnet.NewEngine(ctx, securejsonnet.EngineConfig{})
if err != nil {
	log.Fatal(err)
}
defer engine.Close(context.Background())

imports, err := securejsonnet.NewMapImporter(map[string][]byte{
	"lib/data.jsonnet": []byte(`{answer: 42}`),
})
if err != nil {
	log.Fatal(err)
}

result, err := engine.Evaluate(ctx, securejsonnet.Request{
	Source:   `import "lib/data.jsonnet"`,
	Importer: imports,
})
if err != nil {
	log.Fatal(err)
}
fmt.Print(string(result.Output))
```

`EngineConfig` fields are ceilings. Zero values select conservative defaults:
128 MiB of WASM memory, 256 KiB source and individual imports, 1 MiB output,
2 MiB cumulative imports, 64 import resolutions, 128 capability calls,
512 KiB host-call payloads, and 256 Jsonnet stack frames. Values above the
library's hard ceilings are clamped.

## Imports

The guest always installs a custom importer. It never uses go-jsonnet's
`FileImporter`. If `Request.Importer` is nil, all imports are denied.

`NewMapImporter` is the safe default for virtual files. It copies its input and
only accepts nonempty, canonical, relative UTF-8 paths using `/`. Absolute
paths, backslashes, empty or dot segments, and all `..` traversal are denied.
Custom importers are trusted host code, receive the evaluation context, and
must return stable content for a canonical path.

## Capabilities

Capabilities are request-scoped Jsonnet native functions:

```go
Capabilities: map[string]securejsonnet.Capability{
	"lookup": {
		Params: []string{"key"},
		Call: func(ctx context.Context, args []any) (any, error) {
			return records[args[0].(string)], nil
		},
	},
}
```

Jsonnet calls them with `std.native("lookup")("key")`. Arguments and results
cross the sandbox as JSON-compatible values. Handler panics become typed
capability errors.

Capabilities must be pure, deterministic queries. Jsonnet is lazy, so a native
function may execute zero, one, or multiple times. Effectful operations are
unsupported. The library cannot prove purity; this is a contract imposed on
trusted handlers.

## Security model

Jsonnet source is adversarial. Importers and capability implementations are
trusted. Each evaluation:

- instantiates and initializes a fresh anonymous WASI guest;
- uses a precompiled module but no shared guest state;
- validates every ABI function, pointer, length, and integer conversion;
- exposes no filesystem, environment, arguments, network, or inherited
  standard streams;
- applies source, output, import, host-call, capability, stack, and linear
  memory limits; and
- closes and discards the guest after success, error, cancellation, or trap.

Wazero has no deterministic instruction-fuel budget. CPU control therefore
uses the caller's context deadline with `WithCloseOnContextDone(true)`.
Cancellation terminates guest execution instead of abandoning a goroutine.
Trusted host handlers run as host Go code and must honor their context; a
handler that blocks while ignoring cancellation can still block its call.

`Engine.Close` is idempotent. It rejects new evaluations and closes the wazero
runtime, aborting active guest calls.

## Rebuilding the guest

The embedded reactor is generated with Go 1.24.5:

```sh
make wasm
make wasm-check
```

The build uses `CGO_ENABLED=0`, `GOOS=wasip1`, `GOARCH=wasm`,
`-buildmode=c-shared`, `-trimpath`, and a cleared build ID. CI rebuilds the
module and verifies both its bytes and checked-in SHA-256 checksum.
