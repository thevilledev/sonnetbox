# securejsonnet

`securejsonnet` evaluates untrusted Jsonnet in a fresh WebAssembly sandbox. It
uses the pure-Go
[go-jsonnet](https://github.com/google/go-jsonnet) evaluator inside a Go WASI
reactor and the pure-Go [wazero](https://wazero.io/) runtime in the host. It
does not use Cgo, C++, Wasmtime, shared libraries, or go-jsonnet's browser-only
`js/wasm` artifact.

The module requires Go 1.24.5 or newer because that is go-jsonnet's minimum.
The checked-in WASM guest is built with the current supported toolchain pinned
in the Makefile.

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
512 KiB host-call payloads, and 256 Jsonnet stack frames. Invalid values and
values above the library's hard ceilings are rejected rather than silently
changed. Memory limits must be multiples of the 64 KiB WASM page size, and
nonzero host-response limits must be at least 256 bytes.

Set `Request.StringOutput` to return a top-level Jsonnet string without JSON
quoting. Set `Request.OmitTrailingNewline` to disable go-jsonnet's default
trailing newline.

## Imports

The guest always installs a custom importer. It never uses go-jsonnet's
`FileImporter`. If `Request.Importer` is nil, all imports are denied.

`NewMapImporter` is the safe default for virtual files. It copies its input and
only accepts nonempty, canonical, relative UTF-8 paths using `/`. Absolute
paths, backslashes, empty or dot segments, and all `..` traversal are denied.
Custom importers are trusted host code, receive the evaluation context, and
must return stable content for a canonical path. The engine applies the same
per-import and cumulative byte limits to every importer. Custom importers may
be called concurrently by separate evaluations.

Import content crosses the ABI as base64 inside a strict JSON envelope. The
encoded envelope, including base64 expansion and the canonical path, must fit
within `MaxHostResponseBytes`.

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
trusted handlers. Handlers may run concurrently in separate evaluations and
must honor context cancellation.

## ABI

The embedded guest uses ABI version 2. The host writes one bounded JSON
evaluation request into guest-owned memory and invokes the guest. Guest imports
are limited to WASI Preview 1 and one function:

```text
securejsonnet_host.call(operation, requestPtr, requestLen, responsePtr, responseCapacity) uint64
```

Operation 1 resolves an import and operation 2 invokes a capability. The high
32 bits of the result are the status and the low 32 bits are the bytes written.
Statuses are success, denial, handler failure, limit exceeded, cancellation,
and malformed request.

The guest allocates one bounded response buffer lazily and reuses it during an
evaluation. The host never calls back into guest allocators. Host callbacks
copy requests before calling trusted handlers, validate the complete response
range, and never retain guest-memory views after returning.

## Security model

Jsonnet source is adversarial. Importers and capability implementations are
trusted. Each evaluation:

- instantiates and initializes a fresh, uniquely named WASI guest;
- uses a precompiled module but no shared guest state;
- validates every ABI function, pointer, length, and integer conversion;
- exposes no filesystem, environment, arguments, network, or inherited
  standard streams;
- applies source, output, import, host-call, capability, stack, and linear
  memory limits; and
- closes and discards the guest after success, error, cancellation, or trap.

The output byte limit is checked after go-jsonnet has rendered the result
because go-jsonnet returns a complete string. The linear-memory ceiling bounds
transient rendering allocations that occur before that check.

Wazero has no deterministic instruction-fuel budget. CPU control therefore
uses the caller's context deadline with `WithCloseOnContextDone(true)`.
Cancellation terminates guest execution instead of abandoning a goroutine.
Trusted host handlers run as host Go code and must honor their context; a
handler that blocks while ignoring cancellation can still block its call.

The sandbox protects the host from adversarial Jsonnet source. It does not
protect against malicious importers or capability implementations: those are
ordinary trusted Go code in the host process. Capabilities that perform writes,
network mutations, or exactly-once effects are unsupported.

`Engine.Close` is idempotent. It rejects new evaluations and closes the wazero
runtime, aborting active guest calls.

## Rebuilding the guest

The embedded reactor is generated with the exact Go version recorded by
`GO_TOOLCHAIN` in the Makefile:

```sh
make wasm
make wasm-check
```

The build uses `CGO_ENABLED=0`, `GOOS=wasip1`, `GOARCH=wasm`,
`-buildmode=c-shared`, `-trimpath`, and a cleared build ID. CI rebuilds the
module and verifies both its bytes and checked-in SHA-256 checksum.
