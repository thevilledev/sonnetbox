# securejsonnet

[![CI](https://github.com/thevilledev/wasmnet/actions/workflows/ci.yml/badge.svg)](https://github.com/thevilledev/wasmnet/actions/workflows/ci.yml)

`securejsonnet` evaluates untrusted Jsonnet in a fresh WebAssembly sandbox. It
embeds [go-jsonnet](https://github.com/google/go-jsonnet) in a Go WASI guest
and runs that guest with [wazero](https://wazero.io/). Jsonnet receives no
ambient filesystem, environment, network, arguments, or inherited standard
streams. The host explicitly supplies imports, pure native capabilities, and
resource budgets.

Use the core API for new integrations. Existing go-jsonnet applications can
start with the opt-in `compat/gojsonnet` package, which keeps the familiar VM
workflow while adding contexts and an explicit sandbox boundary.

The current host API is for Go. C++/libjsonnet applications need a Go service
or sidecar boundary; there is not yet a C ABI or drop-in `jsonnet` CLI.

The module requires Go 1.25.12 or newer. It uses no Cgo, C++, Wasmtime, shared
libraries, or go-jsonnet's browser-only `js/wasm` artifact.

## Quick start

The module path is `github.com/thevilledev/wasmnet`; its root package name is
`securejsonnet`:

```go
import securejsonnet "github.com/thevilledev/wasmnet"

engine, err := securejsonnet.NewEngine(
	context.Background(),
	securejsonnet.EngineConfig{},
)
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

ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

result, err := engine.Evaluate(ctx, securejsonnet.Request{
	Filename: "apps/main.jsonnet",
	Source:   `import "../lib/data.jsonnet"`,
	Importer: imports,
})
if err != nil {
	log.Fatal(err)
}
fmt.Print(string(result.Output))
```

Create one long-lived `Engine` per policy profile. It compiles the embedded
module once, is safe for concurrent evaluation, and creates a fresh guest for
every request. Do not create an engine per evaluation.

For a current go-jsonnet codebase, see [MIGRATING.md](MIGRATING.md) for the
compatibility contract, before-and-after code, supported API matrix, and
rollout checklist.

## Evaluation

The three input entry points have deliberately different import behavior:

- `Evaluate` evaluates inline source relative to `Request.Filename`.
- `EvaluateAnonymous` matches go-jsonnet's anonymous-snippet behavior:
  `Filename` is diagnostic only and imports start at the importer root.
- `EvaluateFile` loads the root file through `Request.Importer`; it never opens
  the path in the guest.

`Request.OutputMode` selects one JSON value, a multi-file object, or a document
stream. Results are returned in `Result.Output`, `Result.Files`, or
`Result.Documents`. `StringOutput` unquotes top-level strings, and
`OmitTrailingNewline` disables go-jsonnet's normal output newline.

External variables, external code, top-level arguments, and top-level code
are request-scoped. Requests do not share mutable Jsonnet VM state.

## Imports

If `Request.Importer` is nil, all imports are denied. The guest never uses
go-jsonnet's `FileImporter`.

`NewMapImporter` is the safest option for an immutable virtual file set.
`NewWorkspaceImporter` exposes a read-only host directory while preventing
relative paths and symlinks from escaping it:

```go
workspace, err := securejsonnet.NewWorkspaceImporter(
	"./jsonnet",
	securejsonnet.WithLibraryPaths("vendor", "lib"),
)
if err != nil {
	log.Fatal(err)
}
defer workspace.Close()
```

Library paths follow go-jsonnet `FileImporter` precedence. Virtual paths use
canonical `/` separators. Normal relative imports such as `./lib.jsonnet` and
`../shared.libsonnet` work when their resolved path remains inside the virtual
root. Absolute paths, backslashes, volume-qualified paths, and root escapes
are denied.

Custom importers are trusted host code. They receive the evaluation context,
must return stable content for a canonical path, and must be safe for calls
from concurrent evaluations. The engine applies per-import, cumulative import,
and host-message byte limits to every importer.

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

Jsonnet calls the example with `std.native("lookup")("key")`. Arguments and
results must be JSON-compatible. Panics and errors become typed capability
errors.

Capabilities must be pure, deterministic queries. Jsonnet is lazy, so a
native function may execute zero, one, or multiple times. Effectful operations
and exactly-once semantics are unsupported. Handlers are trusted host code,
may run concurrently across evaluations, and must honor cancellation.

## Budgets and observability

`EngineConfig` defines ceilings for the engine. Zero fields select these
defaults:

| Ceiling | Default |
| --- | ---: |
| Guest linear memory | 128 MiB |
| Source / one import | 256 KiB each |
| Rendered output | 1 MiB |
| Cumulative imports | 2 MiB |
| Import resolutions | 64 |
| Capability calls | 128 |
| Host request / response | 512 KiB each |
| Captured trace | 64 KiB |
| Jsonnet stack | 256 frames |
| Concurrent evaluations | 4 |

Invalid values and values above the library's hard ceilings are rejected.
`Request.Limits` can lower every per-evaluation ceiling except memory and
concurrency; it can never raise the engine policy. Use a context deadline as
the CPU budget. Evaluations waiting for a concurrency slot also honor
cancellation.

Set `Request.CaptureTrace` to collect bounded `std.trace` output in
`Result.Trace`. `Result.Stats` reports queue and execution duration, import
count and bytes, capability calls, trace bytes, and trace truncation. It is
available on successful evaluations and is host-observed diagnostic data, not
a billing or deterministic instruction counter.

`Version()` reports the embedded go-jsonnet version and the private
host/guest ABI version, which is useful in logs and compatibility reports.

## Compatibility contract

The embedded evaluator is go-jsonnet v0.22.0. For successful evaluations on
the documented compatibility surface and within configured budgets,
securejsonnet intends to return the same rendered bytes as that version.
Differential tests cover variables, arguments, native functions, imports,
traces, single output, multi-file output, and streams.

The contract does not promise identical concrete error types or strings,
performance, mutable VM behavior, arbitrary newer go-jsonnet behavior, AST
APIs, a debugger, or unrestricted filesystem imports. Security policy always
wins over compatibility. See [MIGRATING.md](MIGRATING.md) for the precise
matrix and known migration work.

## Security model

Jsonnet source is adversarial. Importers and capability implementations are
trusted. Each evaluation:

- instantiates and initializes a fresh, uniquely named WASI guest;
- uses a precompiled module but no shared guest state;
- validates every ABI function, pointer, length, and integer conversion;
- exposes no ambient filesystem, environment, arguments, network, or
  inherited standard streams;
- applies source, output, import, trace, host-call, capability, stack, linear
  memory, and concurrency limits; and
- closes and discards the guest after success, error, cancellation, or trap.

The output limit is checked after go-jsonnet renders the complete result. The
linear-memory ceiling bounds transient rendering allocations before that
check.

Wazero has no deterministic instruction-fuel budget. CPU control uses the
caller's context deadline with `WithCloseOnContextDone(true)`, which
terminates guest execution. A trusted host handler that ignores cancellation
can still block its call.

The sandbox protects the host from adversarial Jsonnet, not from malicious
importers or capabilities running as ordinary Go code. `Engine.Close` is
idempotent, rejects new work, and aborts active guest calls.

## ABI

The embedded guest uses private ABI version 5. The host sends one bounded
evaluation request to guest-owned memory. Guest-to-host calls use one imported
function for import resolution and capability invocation. Status values
distinguish success, denial, handler failure, limits, cancellation, and
malformed messages.

The guest exposes bounded result and trace buffers. Host callbacks copy
requests before invoking trusted handlers, validate complete memory ranges,
and never retain guest-memory views. The ABI is an internal implementation
detail and is not a public extension point.

## Rebuilding and development

The embedded reactor is generated with the exact Go version in `.go-version`:

```sh
make wasm
make wasm-check
```

The build uses `CGO_ENABLED=0`, `GOOS=wasip1`, `GOARCH=wasm`,
`-buildmode=c-shared`, `-trimpath`, and a cleared build ID. CI rebuilds the
module and verifies its bytes and checked-in SHA-256 checksum.

Run `make check` for formatting, module, lint, coverage, portability, and WASM
reproducibility checks. `make race` and `make fuzz-smoke` provide the extended
checks.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the local workflow and
[SECURITY.md](SECURITY.md) for private vulnerability reporting.
