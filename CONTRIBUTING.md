# Contributing

## Development setup

Use Go 1.25 or newer. The supported development and Wasm build toolchain is
pinned in `.go-version`; golangci-lint is pinned in
`.golangci-lint-version`.

Before sending a change, run:

```sh
make check
make race
make fuzz-smoke
```

`make check` verifies formatting, module metadata, lint, coverage, the
Cgo-free dependency graph, and the checked-in Wasm guest. CI runs the same
checks as independent jobs.

To apply the configured Go formatters, run `make fmt`.

## Updating the embedded guest

Changes to guest code or guest dependencies require rebuilding the checked-in
Wasm reactor:

```sh
make wasm
make wasm-check
```

Commit both `internal/guestblob/securejsonnet.wasm` and its `.sha256` file.
The build is reproducible with the toolchain pinned in `.go-version`.

## Changes and reviews

Keep changes focused and add tests for observable behavior. Use Conventional
Commit subjects, sign commits, and include a `Signed-off-by` trailer.
Pull requests must pass every required CI job before merge.
