# Migrating from go-jsonnet

`securejsonnet` replaces the execution boundary around untrusted Jsonnet. It
does not try to replace every parser, AST, formatter, or editor feature in
go-jsonnet.

The shortest safe migration is:

1. create one long-lived secure engine;
2. replace ambient filesystem access with an explicit workspace or virtual
   importer;
3. use the compatibility VM to minimize call-site changes;
4. add deadlines and request budgets;
5. differential-test representative production inputs; and
6. move to the core request API when request-scoped policy and telemetry are
   useful.

## Minimal VM migration

A typical go-jsonnet integration might look like this:

```go
vm := jsonnet.MakeVM()
vm.Importer(&jsonnet.FileImporter{
	JPaths: []string{"jsonnet/vendor"},
})
vm.ExtVar("environment", environment)
output, err := vm.EvaluateFile("jsonnet/apps/main.jsonnet")
```

The compatibility path keeps the same conceptual flow:

```go
engine, err := securejsonnet.NewEngine(
	context.Background(),
	securecompat.RecommendedEngineConfig(),
)
if err != nil {
	return err
}
defer engine.Close(context.Background())

workspace, err := securejsonnet.NewWorkspaceImporter(
	"jsonnet",
	securejsonnet.WithLibraryPaths("vendor"),
)
if err != nil {
	return err
}
defer workspace.Close()

vm, err := securecompat.New(engine)
if err != nil {
	return err
}
vm.Importer(workspace)
vm.ExtVar("environment", environment)

ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()
output, err := vm.EvaluateFile(ctx, "apps/main.jsonnet")
```

The imports are:

```go
import (
	securejsonnet "github.com/thevilledev/wasmnet"
	securecompat "github.com/thevilledev/wasmnet/compat/gojsonnet"
)
```

The important changes are the context parameter, long-lived engine, and
explicit workspace root. `RecommendedEngineConfig` raises the secure default
stack ceiling from 256 to go-jsonnet's default of 500 while retaining every
other secure default.

## Compatibility matrix

| go-jsonnet usage | Migration support | Important difference |
| --- | --- | --- |
| `EvaluateAnonymousSnippet` | Compatibility VM | Adds `context.Context`; imports start at virtual root |
| `EvaluateFile` | Compatibility VM | Root file must come through an explicit importer |
| Multi and stream variants | Compatibility VM | Adds context; same rendered shapes |
| `ExtVar`, `ExtCode`, resets | Compatibility VM | Copied into each isolated request |
| `TLAVar`, `TLACode`, resets | Compatibility VM | Copied into each isolated request |
| `StringOutput`, `OutputNewline` | Compatibility VM | Same intent |
| `MaxStack` | Compatibility VM | May only lower the engine ceiling |
| `NativeFunction` | Compatibility VM | Must be pure and JSON-compatible |
| `SetTraceOut` | Compatibility VM | Successful trace is bounded, buffered, then written after evaluation |
| Custom `Importer` | `AdaptImporter` | Trusted adapter serializes importer calls |
| `FileImporter` / `JPaths` | `WorkspaceImporter` | Must declare one root; root escape is denied |
| Parse, AST, formatter, linter | Keep go-jsonnet tooling | Not part of the sandbox runtime API |
| Debugger and custom evaluator internals | No compatibility layer | Requires redesign or trusted pre-processing |
| Dependency discovery | No compatibility layer | Record imports in a custom importer if needed |

The compatibility VM is intentionally not source-compatible: every evaluation
takes a context. That context is the cancellation and CPU-budget boundary and
should not be hidden behind `context.Background()` in request-serving code.

`EvaluateSnippet` and its multi/stream variants are securejsonnet extensions.
Unlike go-jsonnet's anonymous method, they resolve relative imports from the
inline snippet's `Filename`.

## What output parity means

The embedded evaluator is exactly go-jsonnet v0.22.0. For a successful
evaluation that uses the supported surface and remains inside its budgets, the
intended compatibility guarantee is byte-for-byte rendered output parity with
that version. Repository differential tests exercise:

- external strings and code;
- top-level strings and code;
- pure native functions;
- custom imports and file entry points;
- `std.trace`;
- normal, string, and newline settings; and
- single, multi-file, and stream manifestation.

Use `securejsonnet.Version()` to record the evaluator and ABI in diagnostics.
Pin and upgrade securejsonnet deliberately if output stability matters.

The following are not parity guarantees:

- exact Go error types, wrapping, or text;
- stack traces outside go-jsonnet's formatted evaluation details;
- behavior after a configured budget is exhausted;
- behavior from a different go-jsonnet release;
- execution time, allocation profile, or cache behavior;
- timing of trace writes; and
- timing, count, or ordering of effectful native calls.

Trace from a failed evaluation is not currently returned or written by the
compatibility VM.

The final item is why capabilities must be pure. Jsonnet laziness already
makes side effects unsafe, and crossing a sandbox makes retry and cancellation
boundaries more visible.

## The difficult migration work

### Filesystem assumptions

This is usually the largest change. A `FileImporter` can follow process paths,
working-directory assumptions, and broad `JPaths`. Secure execution requires a
declared virtual root.

Use `NewWorkspaceImporter(root, WithLibraryPaths(...))` for a checked-out
repository. Root files and imports are read-only, and symlinks cannot escape
the root. Paths passed to `EvaluateFile` become root-relative virtual paths.

Use `NewMapImporter` when files already live in a database, bundle, request,
or generated map. Use a custom importer for an artifact store or service, but
remember that importer code runs with the host process's authority.

`AdaptImporter` exists for trusted custom go-jsonnet importers during a staged
migration. It deliberately rejects `*jsonnet.FileImporter`; wrapping an unsafe
filesystem importer would defeat the isolation boundary. The old importer API
does not accept a context, so an adapted importer cannot be interrupted while
its `Import` method is running. Prefer a core securejsonnet importer for
request-serving code.

### Native functions

`NativeFunction` is adapted to a request capability for convenience. Audit
every function before migration:

- arguments and results must be JSON-compatible;
- it must be deterministic for the same arguments;
- it must not perform writes or rely on exactly-once execution;
- it should not expose a generic filesystem, network, shell, or secret lookup;
  and
- it must return promptly and honor cancellation when implemented directly as
  a core `Capability`.

The legacy `NativeFunction.Func` does not receive a context. Prefer a core
`Capability` for handlers that can block.

Prefer narrow functions such as `lookup_catalog_entry(id)` over generic
functions such as `http_get(url)` or `read_file(path)`.

### Mutable VM state

The compatibility VM preserves familiar mutation methods, but snapshots their
values into each request. Like go-jsonnet's VM, it must not be mutated or
evaluated concurrently.

The core `Engine` is concurrency-safe. Prefer constructing a complete
`securejsonnet.Request` at the call site when different tenants or jobs need
different variables, imports, capabilities, or budgets.

### Errors

Do not branch on go-jsonnet error strings. Securejsonnet exposes typed errors
for invalid requests, denied imports, importer failures, capability failures,
limits, cancellation, guest traps, Jsonnet evaluation, ABI failures, and a
closed engine. Use `errors.As` and `errors.Is`, including
`errors.Is(err, context.DeadlineExceeded)` through a cancellation wrapper.

### Performance

WASM isolation adds startup and message-copy overhead compared with an
in-process VM. Treat that overhead as the cost of the untrusted-code boundary.

Create the engine at service startup and reuse it. Each evaluation still gets
a fresh guest, so there is no cross-request Jsonnet state. Tune
`MaxConcurrentEvaluations` from load tests; excess work waits with context
cancellation rather than creating unbounded guests. Watch
`Result.Stats.QueueDuration` separately from `ExecutionDuration`.

Benchmark representative files, imports, and capabilities. Tiny snippets
exaggerate fixed sandbox overhead; large manifestations can instead be
dominated by Jsonnet and JSON rendering.

## Policy mapping

Engine ceilings define the largest request the process will accept.
`RequestLimits` can only lower them for a tenant or operation.

| Concern | Engine ceiling | Per-request override |
| --- | --- | --- |
| Guest memory | `MaxMemoryBytes` | None |
| Source bytes | `MaxSourceBytes` | `MaxSourceBytes` |
| Rendered bytes | `MaxOutputBytes` | `MaxOutputBytes` |
| Stack depth | `MaxStack` | `MaxStack` |
| Import count | `MaxImports` | `MaxImports` |
| One / all import bytes | `MaxImportBytes`, `MaxTotalImportBytes` | Same fields |
| Native calls | `MaxCapabilityCalls` | `MaxCapabilityCalls` |
| ABI message bytes | `MaxHostRequestBytes`, `MaxHostResponseBytes` | Same fields |
| Trace bytes | `MaxTraceBytes` | `MaxTraceBytes` |
| Active guests | `MaxConcurrentEvaluations` | None |
| CPU / wall time | Caller context | Shorter child context |

A zero request field inherits its engine ceiling. A request above its ceiling
is rejected instead of silently clamped.

## Suggested rollout

Start by inventorying each VM configuration, importer, native function, output
mode, and dependency on parser or AST APIs. Keep non-execution tooling on
go-jsonnet if it does not handle adversarial evaluation.

Build a corpus of real programs and compare native and secure rendered bytes.
Include errors, import traversal attempts, large outputs, recursion, trace
volume, and deadline expiry. Move filesystem imports to a workspace root
before enabling untrusted traffic.

Run both evaluators in shadow mode for trusted inputs if the application can
afford it. Compare successful output bytes, but classify expected policy
rejections separately from semantic mismatches. Native functions used during
shadow evaluation must still be pure.

Finally, route untrusted evaluation only through securejsonnet, alert on
limits and cancellation, and tune concurrency from queue duration and memory
measurements. Keep the native evaluator only for trusted tooling that needs
unsupported APIs.

## C++ and CLI callers

The repository currently provides a Go host API, not a drop-in replacement for
libjsonnet or the `jsonnet` executable. A C++ application can adopt the
sandbox today by calling a small Go service or sidecar whose API accepts
source, virtual files, variables, arguments, and a policy identifier.

That service boundary should not accept arbitrary host paths or generic native
functions. Treat imports and capabilities as named server-side policies. A
stable C ABI or compatible CLI would be a separate product surface and is not
currently guaranteed.
