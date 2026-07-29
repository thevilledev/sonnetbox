# sonnetbox-wasmtime

This crate is the Wasmtime reference host for the public sonnetbox ABI. It
loads the release `sonnetbox.wasm` artifact, validates ABI 7, and evaluates
untrusted Jsonnet in a fresh store and instance for every request.

```rust,no_run
use std::time::Duration;
use sonnetbox_wasmtime::{Engine, EngineConfig, Request};

let guest = std::fs::read("sonnetbox.wasm")?;
let engine = Engine::new(&guest, EngineConfig::default())?;
let result = engine.evaluate(
    Request {
        source: "{ answer: 6 * 7 }".into(),
        ..Request::default()
    },
    Duration::from_secs(2),
)?;
assert_eq!(String::from_utf8(result.output)?, "{\n   \"answer\": 42\n}\n");
# Ok::<(), Box<dyn std::error::Error>>(())
```

The default WASI context has no arguments, environment, preopened
directories, network, or inherited standard streams. Memory, Wasm stack,
Jsonnet stack, source, output, imports, capabilities, host messages, trace,
concurrency, deterministic instruction fuel, and wall-clock duration are all
bounded.

Fuel uses the `wasmtime-fuel-v1` model and is not interchangeable with the Go
host's `wazero-fuel-v1` values. Each store also receives deterministic WASI
randomness and clocks so the same guest and request consume repeatable fuel.
The guest does not expose randomness or time to Jsonnet; these imports only
support the compiled Go runtime. Deadline enforcement uses a 10 ms Wasmtime
epoch tick and is rechecked around trusted import and capability callbacks.

`MapImporter` is the simplest filesystem-free import policy. Custom importers
and capabilities are trusted host code: they receive a `HostContext` and must
return promptly when its deadline expires.

The crate is kept in the sonnetbox repository and is not published to
crates.io yet. Tagged releases publish the raw Wasm guest and checksum that a
Rust application should pin and verify.
