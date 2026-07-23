# Repository instructions for agents

These instructions apply to the entire repository.

## Toolchain sources of truth

- `go.mod` declares the minimum supported Go patch release.
- `.go-version` declares the current development and WASM build toolchain.
- These versions may differ intentionally. Do not replace either one with a
  minor-version wildcard.
- The `test-minimum` job in `.github/workflows/ci.yml` must pin the exact
  version declared by `go.mod`.
- The checked-in WASM guest must be rebuilt with the exact version in
  `.go-version`.

Whenever either Go version changes, inspect `go.mod`, `.go-version`, the
`test-minimum` workflow job, and the WASM build configuration together.

## Rebuild the embedded WASM first

After every change to Go code, `go.mod`, or `go.sum`, run:

```sh
make wasm
```

Run this before Go tests, `make check`, or `make ci` so all validation uses the
fresh embedded guest. Do this even when a change appears host-only; let the
deterministic build prove that the guest bytes are unchanged instead of
guessing whether the changed package reaches the guest.

`make wasm-check` only verifies the checked-in artifact. It does not update a
stale artifact and is not a substitute for `make wasm`.

If rebuilding changes the guest, include both of these generated files in the
same commit as the source change:

- `internal/guestblob/wasmnet.wasm`
- `internal/guestblob/wasmnet.wasm.sha256`

Never commit a Go code or dependency change until `make wasm` has run and the
resulting blob and checksum have been inspected.

## Validation before pushing

Treat `.github/workflows/ci.yml` as the source of truth for remote CI. Read it
before claiming that local validation covers CI.

For changes to Go code, dependencies, the Makefile, workflows, or the embedded
WASM, run the local suite:

```sh
GOCACHE=/private/tmp/wasmnet-gocache \
GOLANGCI_LINT_CACHE=/private/tmp/wasmnet-lint-cache \
make ci
```

Then test the exact minimum Go version separately. `make ci` uses the current
toolchain and does not cover this job:

```sh
minimum_go="$(go list -m -f '{{.GoVersion}}')"
GOCACHE=/private/tmp/wasmnet-go-min-cache \
GOTOOLCHAIN="go${minimum_go}" \
CGO_ENABLED=0 \
go test -count=1 -timeout=5m ./...
```

If the minimum toolchain is not installed, allow Go to download that exact
patch release. Do not fall back to an older patch from the same minor line.

`make fuzz-smoke` must use a deterministic iteration budget. Keep time-based
fuzzing in the nightly workflow, where longer execution and wall-clock
variance are expected. Do not fix a fuzz timeout by merely rerunning a failed
job.

The local suite does not execute every GitHub-only integration, including the
hosted vulnerability and workflow-security actions. Call it the "local
suite," not "CI-equivalent validation." State which checks were not run
locally.

Documentation-only changes may use proportionate local validation, but any
edit to workflow commands, documented versions, compatibility guarantees, or
release instructions must follow the full validation above.

## Commits

- Keep independent causes and fixes in separate Conventional Commits.
- Every commit needs a body explaining what and why.
- DCO-sign and GPG-sign every commit with `git commit -sS`.
- Do not stage unrelated or user-owned worktree changes.
- Verify the stored message, signature, and worktree state after committing.

## Pushing and remote CI

Push only when the user asks. Before pushing, confirm the intended branch,
remote, clean worktree, and commits.

After every push to `main`:

1. Locate the workflow run for the exact pushed commit SHA.
2. Monitor it until GitHub reports a terminal conclusion.
3. Do not report the push as fully complete while CI is queued or running.
4. If CI fails, inspect the failed job logs before changing code.
5. Reproduce the exact failing toolchain and command locally when possible.
6. Fix the cause in a focused commit, push it, and monitor the replacement
   run through completion.

Use `gh run view <run-id> --log-failed` for diagnosis and
`gh run watch <run-id> --exit-status` for monitoring. A successful local run
is not evidence that a remote failure is flaky.
