# The forked wazero dependency

`sonnetbox` depends on `github.com/thevilledev/wazero` rather than
`github.com/tetratelabs/wazero`. This note records why, what that costs, and
what an optional upstream build would look like. It is an assessment, not a
committed plan: no port work is scheduled yet.

## Why the fork exists

The fork adds `experimental/fuel`, a deterministic instruction budget. Fuel is
what makes `MaxFuel` a real ceiling and `Result.Stats.FuelConsumed` a
reproducible number: the same program with the same inputs consumes the same
fuel on every run, on every machine. That property is the difference between a
budget an operator can reason about and a wall-clock timeout that varies with
load.

Upstream declined instruction metering in tetratelabs/wazero#422, with the
reasoning developed in PR #1108. Upstreaming is therefore not a realistic
path, and the fork is not a temporary state on the way to one.

## What the fork actually costs

The divergence is not a thin patch that could be rebased cheaply or kept in a
small vendored overlay. Metering has to exist in both execution engines and in
the compiler that feeds them, so the fork changes:

- `internal/engine/wazevo/frontend/lower.go` and `frontend.go`, which lower
  Wasm into SSA, to emit the fuel decrement
- `internal/engine/wazevo/call_engine.go`, `engine.go`, and the
  `wazevoapi` exit codes and offset data, to trap when the budget runs out
- `internal/engine/interpreter/compiler.go`, `interpreter.go`, and
  `operations.go`, to do the same on the portable interpreter
- `internal/wasm/fuel.go`, `internal/wasm/engine.go`,
  `internal/expctxkeys/fuel.go`, `internal/wasmruntime/errors.go`, and
  `runtime.go`, to carry the meter and its error

Two consequences follow. Rebasing onto a new upstream release is real work
across the compiler backend, not a mechanical merge. And for an organisation
with a no-forks policy, this dependency is a hard stop that no amount of
documentation resolves.

## The seam in sonnetbox

The good news is that sonnetbox itself barely touches fuel. Five call sites
cover the whole surface:

| Location | Call | Purpose |
| --- | --- | --- |
| `engine.go:202` | `fuel.WithEnabled(ctx)` | Compile metered code into the runtime |
| `engine.go:235` | `fuel.WithFuel(ctx, math.MaxUint64)` | Unmetered context for the ABI probe |
| `engine.go:395` | `fuel.WithFuel(callCtx, MaxFuel)` | The per-evaluation budget |
| `engine.go:463` | `MaxFuel - meter.Remaining()` | `Stats.FuelConsumed` |
| `engine.go:1658` | `errors.Is(err, fuel.ErrOutOfFuel)` | Classify the trap as a `LimitError` |

A build-tagged internal shim package could satisfy all five against upstream
wazero: `WithEnabled` and `WithMeter` return the context unchanged, `WithFuel`
returns a no-op meter, and `ErrOutOfFuel` is an unexported sentinel that no
error ever wraps.

One of the five is not mechanical. `Stats.FuelConsumed` is computed as
`MaxFuel - meter.Remaining()`, so a meter that reports zero remaining fuel
would report the entire budget as consumed — the most misleading value
possible. The shim build has to report zero consumed instead, which means the
subtraction moves behind the shim rather than staying in `engine.go`.

`WithCloseOnContextDone(true)` at `engine.go:201` is already unconditional, so
the context-deadline backstop needs no change and exists in both builds today.

## The policy downgrade, stated plainly

An upstream build is strictly weaker, and the difference has to be documented
where an operator sets policy, not only here:

- `MaxFuel` is inert. Setting it constrains nothing, and neither
  `EngineConfig.MaxFuel` nor `RequestLimits.MaxFuel` can stop a runaway
  program.
- `Stats.FuelConsumed` is always zero, so the number operators use to size
  budgets from representative programs disappears.
- Termination rests entirely on the context deadline and
  `WithCloseOnContextDone`. That is wall-clock, so it is sensitive to machine
  speed, CPU contention, and noisy neighbours. A program that finishes inside
  the deadline on an idle machine can be killed on a loaded one, and the same
  evaluation is no longer reproducible across hosts.
- Memory ceilings, import ceilings, capability ceilings, output ceilings, and
  every isolation property other than instruction budgeting are unchanged.

The last point is worth stating because it bounds the damage: an upstream build
loses deterministic budgeting, not the sandbox. Untrusted Jsonnet still gets no
filesystem, environment, network, or ambient host access.

## What has to be true before doing the port

1. A named user or organisation that cannot take the fork. Optionality has an
   ongoing cost — two build configurations, two CI matrices, and a policy
   surface that means different things in each — and that cost should be paid
   for a real blocker, not a hypothetical one.
2. A decision on how the weaker build is labelled. Silently accepting
   `MaxFuel` and reporting zero consumption is the wrong default; a build that
   rejects a nonzero `MaxFuel`, or logs once that it is inert, is more honest.
3. A CI job that builds and tests the upstream configuration. The fuel-specific
   tests in `engine_test.go` and `internal/cli/policy_test.go` assert trapping
   and consumption, so they need build tags or a capability guard rather than
   being deleted.
4. Confirmation that the guest still compiles and behaves identically without
   metering, since fuel instrumentation changes the code the backend emits.

Until at least the first of these is real, the fork stays the only supported
configuration and this note stays the record of the trade.
