# sonnetbox

> **Wazero:** this project depends on
> [`github.com/thevilledev/wazero`](https://github.com/thevilledev/wazero/commit/2a752f24e7e1056d7f1f017eba9ca9d755e34493)
> (`feat/fuel-module-path`, based on wazero v1.12.0), a fork that adds the
> deterministic `experimental/fuel` metering this sandbox needs. The fork
> declares its own module path so that `go install` and ordinary module
> resolution work; a `replace` directive would be ignored for anyone
> depending on `sonnetbox`. [docs/wazero-fork.md](docs/wazero-fork.md) records
> why upstreaming is not a path and what an optional upstream build would cost.

[![CI](https://github.com/thevilledev/sonnetbox/actions/workflows/ci.yml/badge.svg)](https://github.com/thevilledev/sonnetbox/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/thevilledev/sonnetbox.svg)](https://pkg.go.dev/github.com/thevilledev/sonnetbox)

`sonnetbox` evaluates untrusted Jsonnet in a fresh WebAssembly sandbox. It
embeds [go-jsonnet](https://github.com/google/go-jsonnet) in a Go WASI guest
and runs that guest with [wazero](https://wazero.io/). Jsonnet receives no
ambient filesystem, environment, network, arguments, or inherited standard
streams. The host explicitly supplies imports, pure native capabilities, and
resource budgets.

Use the core API for new integrations. Existing go-jsonnet applications can
start with the opt-in `compat/gojsonnet` package, which keeps the familiar VM
workflow while adding contexts and an explicit sandbox boundary.

The primary host API is for Go, and the `sonnetbox` command provides a secure,
intentionally bounded subset of the `jsonnet` CLI workflow. It is not a
drop-in replacement. Other language hosts can embed the released guest through
the [portable host ABI](docs/abi.md); they must reproduce the host-side policy,
not merely instantiate the module. C++/libjsonnet applications can also use the
CLI, a Go service, or a sidecar boundary; there is not yet a C ABI.

The module requires Go 1.25.12 or newer. It uses no Cgo, C++, Wasmtime, shared
libraries, or go-jsonnet's browser-only `js/wasm` artifact.

## Quick start

The module path is `github.com/thevilledev/sonnetbox`; its root package name is
`sonnetbox`:

```go
import "github.com/thevilledev/sonnetbox"

engine, err := sonnetbox.NewEngine(
	context.Background(),
	sonnetbox.EngineConfig{},
)
if err != nil {
	log.Fatal(err)
}
defer engine.Close(context.Background())

imports, err := sonnetbox.NewMapImporter(map[string][]byte{
	"lib/data.jsonnet": []byte(`{answer: 42}`),
})
if err != nil {
	log.Fatal(err)
}

ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

result, err := engine.Evaluate(ctx, sonnetbox.Request{
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

Compiling the guest dominates `NewEngine`. A process that cannot keep an
engine alive, such as a short-lived command, should reuse compiled code
through a cache:

```go
cache, err := sonnetbox.NewCompilationCacheDir(cacheDir)
if err != nil {
	log.Fatal(err)
}
defer cache.Close(context.Background())

engine, err := sonnetbox.NewEngine(
	context.Background(),
	sonnetbox.EngineConfig{},
	sonnetbox.WithCompilationCache(cache),
)
```

A cache directory holds executable machine code that later runs in the
process, so point it at a location only the current user can write.
`WithInterpreter` is the alternative for a single evaluation: it starts in
roughly a tenth of the time but evaluates several times slower.

`WithDefaultImporter` and `WithDefaultCapabilities` attach an import policy
and a native function set to the engine itself, so every request gets them
without repeating the wiring at each call site. A request still wins where the
two overlap, but cannot remove a default it did not replace.

For runnable programs that progress from an inline evaluation to a
request-serving integration, see the [examples](examples/README.md).

For a current go-jsonnet codebase, see [MIGRATING.md](MIGRATING.md) for the
compatibility contract, before-and-after code, supported API matrix, and
rollout checklist.

## Command-line usage

Install the Cgo-free command directly:

```sh
go install github.com/thevilledev/sonnetbox/cmd/sonnetbox@latest
```

Tagged releases also provide Linux, macOS, and Windows archives for amd64 and
arm64. Each archive has an SPDX JSON SBOM and is covered by a keyless GitHub
build-provenance attestation. After checking the downloaded archive against
`checksums.txt`, verify its provenance with:

```sh
gh attestation verify --owner thevilledev sonnetbox_*.tar.gz
```

Multi-arch container images (`linux/amd64`, `linux/arm64`) publish to
`ghcr.io/thevilledev/sonnetbox` on the same tags. Each image is Cosign
keyless-signed and carries GitHub provenance plus SPDX SBOM attestations:

```sh
docker pull ghcr.io/thevilledev/sonnetbox:0.1.0
gh attestation verify \
  --owner thevilledev \
  oci://ghcr.io/thevilledev/sonnetbox:0.1.0
cosign verify \
  --certificate-identity-regexp='https://github.com/thevilledev/sonnetbox/\.github/workflows/release\.yml@refs/tags/v.*' \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/thevilledev/sonnetbox:0.1.0
```

Or build `build/sonnetbox` / a local image from a checkout:

```sh
make cli
make docker
```

Evaluate a file. Without `--root`, the file's containing directory is the
entire read-only import workspace:

```sh
sonnetbox config/main.jsonnet
```

Grant a larger workspace explicitly when the program imports from parent or
library directories. The entry filename and `-J` paths are interpreted
relative to that root, and the rightmost library path wins:

```sh
sonnetbox \
  --root ./jsonnet \
  -J vendor \
  -J lib \
  -V environment=production \
  apps/main.jsonnet
```

Inline source and stdin have no importer unless `--root` is supplied:

```sh
sonnetbox -e -S '"hello"'
printf '%s\n' 'import "data.libsonnet"' |
  sonnetbox --root ./jsonnet -
```

The familiar output modes are available:

```sh
sonnetbox -o result.json config.jsonnet
sonnetbox -m generated -c files.jsonnet
sonnetbox -y stream.jsonnet
```

Run `sonnetbox --help` for the complete supported flag set. Every evaluation
uses a fresh WASM guest and a five-second deadline configurable with a
positive `--timeout` duration. `std.trace` is bounded and written to stderr.
Multi-file names are source-controlled, so the command rejects absolute,
non-canonical, and traversing names and confines writes beneath the requested
output directory.

Every resource ceiling is adjustable. A flag sets one ceiling, `--policy`
loads a JSON file of them, and a flag wins over the file. Sizes accept
suffixes such as `512KiB` or `16MB`. The library validates the result, so a
policy can only narrow what sonnetbox already permits:

```sh
sonnetbox --max-fuel 20000000 --max-memory 32MiB untrusted.jsonnet
sonnetbox --max-wasm-stack 128MiB deeply-recursive.jsonnet
sonnetbox --policy ./sandbox-policy.json untrusted.jsonnet
```

`--print-policy` writes the effective ceilings as JSON, which is itself a
valid `--policy` file, so operators can capture and version the defaults:

```sh
sonnetbox --print-policy > sandbox-policy.json
```

The command caches compiled guest code beneath the user cache directory, which
takes a warm run from roughly 2.6 seconds to 0.12 seconds. Because the cache
holds executable machine code, it is created private to the current user.
`--cache-dir` moves it and `--no-cache` disables it.

Failures are reported for people by default and for machines on request.
`--error-format=json` writes a structured report naming the error kind, the
exhausted resource and its limit, or the denied import path. Exit status
distinguishes the failure classes: `0` success, `1` usage or host failure, `2`
Jsonnet error, `3` exhausted budget, `4` denied import, and `5` canceled or
timed out.

This is deliberately a secure subset rather than a drop-in `jsonnet`
replacement:

- `JSONNET_PATH` is ignored, and setting it prints a notice naming `-J`;
- `--ext-str`, `--ext-code`, `--tla-str`, and `--tla-code` require
  `name=value` and never infer values from the environment;
- native functions, formatter and linter commands, stack-trace cropping
  (`-t`), garbage-collector tuning, and exact upstream error text are not
  supported, and each is refused by name rather than as an unknown flag; and
- the operator-selected input, workspace, library paths, variable files, output
  file, and output directory are host-side grants, while Jsonnet itself
  receives no ambient host access.

`--ext-str-file`, `--ext-code-file`, `--tla-str-file`, and `--tla-code-file`
are supported because the operator names the file on the command line. The host
reads it directly, unlike upstream `jsonnet`, which resolves it through the
importer and therefore through `JPaths`.

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
workspace, err := sonnetbox.NewWorkspaceImporter(
	"./jsonnet",
	sonnetbox.WithLibraryPaths("vendor", "lib"),
	sonnetbox.WithSearchRoot("stdlib", "/opt/jsonnet-stdlib"),
)
if err != nil {
	log.Fatal(err)
}
defer workspace.Close()
```

`WithLibraryPaths` names directories inside the workspace root.
`WithSearchRoot` grants a directory outside it as a separate read-only root,
which is how a go-jsonnet `FileImporter` with absolute or disjoint `JPaths` is
expressed without widening the workspace. Each search root gets its own
traversal-resistant root, and imports resolved there report paths beneath the
mount name, so `stdlib/k.libsonnet` is distinguishable from a `k.libsonnet` in
the workspace and resolves its own relative imports inside `stdlib`.

Every declaration shares one precedence list, searched in reverse declaration
order after the importing file's own directory, matching `FileImporter`. Virtual
paths use canonical `/` separators. Normal relative imports such as
`./lib.jsonnet` and `../shared.libsonnet` work when their resolved path remains
inside the granted root. Absolute paths, backslashes, volume-qualified paths,
and root escapes are denied.

`Result.Imports` reports the canonical paths an evaluation resolved,
deduplicated and in resolution order, bounded by `MaxImports`. It records what
the evaluation actually needed rather than what the source text mentions, so a
lazily unused import is absent.

The `sonnetbox` CLI maps `-J` onto both forms: a path inside `--root` becomes a
library path, and one outside it becomes a search root named after its last path
element.

Custom importers are trusted host code. They receive the evaluation context,
must return stable content for a canonical path, and must be safe for calls
from concurrent evaluations. The engine applies per-import, cumulative import,
and host-message byte limits to every importer.

## Capabilities

Capabilities are request-scoped Jsonnet native functions:

```go
Capabilities: map[string]sonnetbox.Capability{
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
| Compiled WASM call stack | 50,000,000 bytes |
| Deterministic WASM fuel | 100,000,000 units |
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
`Request.Limits` can lower every per-evaluation ceiling except memory, the
compiled WASM call stack, and concurrency; it can never raise the engine
policy. Fuel deterministically
bounds guest instruction work, while a context deadline remains the wall-clock
backstop for host callbacks and evaluation. Evaluations waiting for a
concurrency slot also honor cancellation.

Policy is a value, not a hidden constant. `DefaultEngineConfig` returns the
table above, `Ceilings` returns the maximum each field accepts, and
`Engine.Config` reports what an engine actually enforces. `EngineConfig`
round-trips through JSON, and `EngineConfig.Normalize` applies and validates a
policy without paying to compile the guest.

Set `Request.CaptureTrace` to collect bounded `std.trace` output in
`Result.Trace`. `Result.Stats` reports deterministic fuel consumed, queue and
execution duration, import count and bytes, capability calls, trace bytes, and
trace truncation. Fuel is an abstract instruction unit, not elapsed time or a
billing unit; the other fields are host-observed diagnostics.

A failed evaluation returns its trace and statistics alongside the error, so
the evidence of why a template failed is not discarded. Nothing is recoverable
when a fuel, memory, or deadline backstop traps the guest, because no further
guest call can succeed.

`WithObserver` reports activity as it happens rather than as a total
afterwards, which is what an audit trail needs. Hooks cover every import
attempt, including the denials that a program probing for ungranted files
would otherwise perform invisibly, every capability call, and every completed
evaluation. `NewSlogObserver` writes them through `log/slog` with denials at
warn level. Events carry paths, sizes, counts, and outcomes but never imported
content or capability arguments, so an audit log cannot become a copy of the
data crossing the sandbox.

`Version()` reports the embedded go-jsonnet version and host/guest ABI version,
which is useful in logs and compatibility reports. Package `guest` exposes a
defensive copy of the same Wasm module attached to tagged releases.

## Compatibility contract

The embedded evaluator is go-jsonnet v0.22.0. For successful evaluations on
the documented compatibility surface and within configured budgets,
sonnetbox intends to return the same rendered bytes as that version.
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
- applies source, output, import, trace, host-call, capability, stack,
  deterministic instruction-fuel, linear-memory, and concurrency limits; and
- closes and discards the guest after success, error, cancellation, or trap.

The output limit is checked after go-jsonnet renders the complete result. The
linear-memory ceiling bounds transient rendering allocations before that
check.

Wazero fuel deterministically terminates guest execution that exceeds the
configured instruction budget. The caller's context deadline, enforced with
`WithCloseOnContextDone(true)`, remains the wall-clock backstop. Trusted host
handlers do not consume guest fuel and can still block if they ignore
cancellation.

A compilation cache stores machine code compiled from the embedded guest and
loads it into the process on a later run, so its directory is part of the
trusted computing base. Anyone who can write there can execute code as the
user running sonnetbox. The command creates its cache private to the current
user; a host passing its own directory must do the same and must never share
one across trust boundaries. `--no-cache` removes the cache from the picture
entirely.

An observer sees paths, sizes, counts, and outcomes, never imported content or
capability arguments, so enabling an audit trail does not widen exposure of
the data crossing the sandbox. Hooks run inline on the evaluation path as
trusted host code.

The sandbox protects the host from adversarial Jsonnet, not from malicious
importers or capabilities running as ordinary Go code. `Engine.Close` is
idempotent, rejects new work, and aborts active guest calls.

## ABI

The embedded guest uses public ABI version 7. The host sends one bounded
evaluation request to guest-owned memory. Guest-to-host calls use one imported
function for import resolution and capability invocation. Status values
distinguish success, denial, handler failure, limits, cancellation, and
malformed messages.

The guest exposes bounded result and trace buffers. Host callbacks copy
requests before invoking trusted handlers, validate complete memory ranges,
and never retain guest-memory views. [docs/abi.md](docs/abi.md) is the
language-neutral contract, with a machine-readable manifest and example
messages under `abi/v7`.

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
checks. `make conformance` compares against the pinned upstream Jsonnet suite,
and `make bench` reports the performance figures published in
[MIGRATING.md](MIGRATING.md).

See [CHANGELOG.md](CHANGELOG.md) for release notes,
[CONTRIBUTING.md](CONTRIBUTING.md) for the local workflow, and
[SECURITY.md](SECURITY.md) for private vulnerability reporting.
