# Portable host ABI

The checked-in `guest/sonnetbox.wasm` is a WASI Preview 1 reactor. It can
run under any WebAssembly runtime that supplies WASI Preview 1 and the host
function described here. The Go package remains the reference host, but the
ABI is public so another host can preserve the same isolation boundary.

ABI 7 is the first version with a public compatibility contract. The
machine-readable manifest and example messages live in `abi/v7`.

## Versioning and artifact

Every host must call `sonnetbox_abi_version` after initialization and reject a
version it does not implement. An incompatible signature, message, framing, or
status change increments the ABI version. The guest bytes may change without an
ABI change when the evaluator or implementation changes, so deployments that
need byte-for-byte stability must also pin the sonnetbox release or SHA-256
digest.

Tagged releases attach `sonnetbox.wasm` and `sonnetbox.wasm.sha256`. The same
bytes are available to Go programs through `guest.Bytes`, which returns a copy
so callers cannot mutate the reference engine's embedded module.

## Module shape and lifecycle

The guest imports WASI functions only from `wasi_snapshot_preview1`. A secure
host supplies empty arguments and environment, no preopened directories, no
network, and no inherited standard streams. The guest also imports:

```text
sonnetbox_host.call(
    operation: i32,
    request_ptr: i32,
    request_len: i32,
    response_ptr: i32,
    response_capacity: i32,
) -> i64
```

The high 32 bits of the return value are a host status and the low 32 bits are
the response length. All integer fields are interpreted as unsigned bit
patterns.

The guest exports one memory named `memory` and these functions:

```text
_initialize() -> ()
sonnetbox_abi_version() -> i32
sonnetbox_request_alloc(size: i32) -> i32
sonnetbox_evaluate() -> i32
sonnetbox_result_ptr() -> i32
sonnetbox_result_len() -> i32
sonnetbox_trace_ptr() -> i32
sonnetbox_trace_len() -> i32
sonnetbox_trace_truncated() -> i32
```

A host compiles the module once, then performs this sequence in a fresh
instance for every evaluation:

1. Instantiate with a unique instance identity and call `_initialize`.
2. Set the runtime's memory, stack, instruction, and deadline limits.
3. Allocate the encoded request with `sonnetbox_request_alloc`.
4. Copy the request into guest memory and call `sonnetbox_evaluate`.
5. Read trace metadata, then result metadata and bytes.
6. Close and discard the instance after every outcome.

The ABI does not permit instance reuse. A host must validate every exported
signature, pointer, length, memory range, status, and integer conversion.
Guest-memory views must not survive a host callback.

## Evaluation request

The allocation contains exactly one UTF-8 JSON object. Unknown fields and
trailing JSON values are invalid. `abi/v7/testdata/evaluation_request.json`
contains a complete example.

| Field | JSON type | Meaning |
| --- | --- | --- |
| `filename` | string | Canonical virtual filename used for diagnostics |
| `source` | string | Inline Jsonnet source |
| `input_mode` | integer | `0` snippet, `1` importer file, `2` anonymous snippet |
| `output_mode` | integer | `0` single, `1` multi-file, `2` stream |
| `ext_vars`, `ext_code` | object of strings | External strings and Jsonnet code |
| `tla_vars`, `tla_code` | object of strings | Top-level strings and Jsonnet code |
| `capabilities` | object | Capability names mapped to `{"params":[...]}` |
| `string_output` | boolean | Unquote top-level strings where supported |
| `output_newline` | boolean | Append go-jsonnet's normal output newline |
| `capture_trace` | boolean | Retain bounded `std.trace` output |
| `limits` | object | Guest-enforced byte, stack, host-message, and trace limits |

The limits object contains `max_output_bytes`, `max_stack`,
`max_host_request_bytes`, `max_host_response_bytes`, and `max_trace_bytes`.
All must be positive and at or below the hard ceilings compiled into the
guest.

## Host callbacks

Operation `1` resolves an import. Its request is
`{"from":string,"path":string}`. Success returns
`{"canonical":string,"content_base64":bytes}`. JSON encoders naturally
represent the byte slice as a base64 string.

Operation `2` invokes a capability. Its request is
`{"name":string,"args":[JSON values...]}`. Success returns
`{"value":JSON value}`.

Callbacks use these statuses:

| Value | Name | Meaning |
| --- | --- | --- |
| `0` | OK | Response contains the successful JSON envelope |
| `1` | Denied | Import or capability was not granted |
| `2` | Handler failure | Trusted host handler failed |
| `3` | Limit | A host-enforced request limit was exceeded |
| `4` | Canceled | Evaluation cancellation reached the handler |
| `5` | Malformed | Guest request or memory range violated the ABI |

For non-OK responses, the payload is a bounded UTF-8 diagnostic. A host must
enforce import count and bytes, capability call count, and both host-message
limits before invoking or returning from trusted handlers.

## Evaluation results

`sonnetbox_evaluate` returns:

| Value | Name | Result payload |
| --- | --- | --- |
| `0` | OK | Manifested output |
| `1` | Invalid request | Structured guest error |
| `2` | Jsonnet error | Structured guest error |
| `3` | Host error | Structured guest error; prefer the recorded host cause |
| `4` | Limit | Structured guest error |
| `5` | Internal | Structured guest error |

A structured guest error is
`{"kind":string,"message":string,"limit"?:integer,"actual"?:integer}`.

Single output is the raw byte range. Multi-file output starts with a
little-endian `u32` count followed by count pairs of length-prefixed UTF-8
filename and output byte strings. Stream output starts with a little-endian
`u32` count followed by that many length-prefixed document byte strings.
Duplicate multi-file names, truncated frames, and trailing bytes are ABI
errors.

Trace bytes are independent of the result and may be available after a
reported evaluation failure. `sonnetbox_trace_truncated` must return only zero
or one.

## Runtime policy and fuel

Instruction fuel is deliberately host-defined rather than part of ABI 7.
Different engines use different deterministic cost schedules, so a limit from
one fuel model must not be silently reused with another. A host must give its
fuel model a stable name, report that name with consumed fuel, and document
whether a policy value belongs to that model.

The official Go host uses `wazero-fuel-v1`. Other hosts must still provide a
deterministic instruction limit and a wall-clock cancellation backstop. A host
that cannot enforce both must describe itself as a weaker compatibility host,
not as an implementation of the sonnetbox sandbox.
