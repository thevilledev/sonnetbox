use std::collections::BTreeMap;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use serde_json::{Value, json};
use sonnetbox_wasmtime::{
    Capability, Engine, EngineConfig, Error, FUEL_MODEL, MapImporter, OutputMode, Request,
    RequestLimits,
};

const GUEST: &[u8] = include_bytes!("../../../guest/sonnetbox.wasm");
const TIMEOUT: Duration = Duration::from_secs(10);

fn engine() -> &'static Engine {
    static ENGINE: OnceLock<Engine> = OnceLock::new();
    ENGINE.get_or_init(|| {
        Engine::new(
            GUEST,
            EngineConfig {
                max_fuel: EngineConfig::ceilings().max_fuel,
                ..EngineConfig::default()
            },
        )
        .expect("build Wasmtime engine")
    })
}

fn json_output(output: &[u8]) -> Value {
    serde_json::from_slice(output).expect("valid JSON output")
}

#[test]
fn evaluates_the_reference_guest() {
    let result = engine()
        .evaluate(
            Request {
                source: "{answer: 6 * 7}".into(),
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("evaluate basic expression");

    assert_eq!(json_output(&result.output), json!({"answer": 42}));
    assert!(result.stats.fuel_consumed > 0);
    assert_eq!(result.stats.fuel_model, FUEL_MODEL);
}

#[test]
fn supports_variables_output_modes_and_jsonnet_errors() {
    let engine = engine();
    let result = engine
        .evaluate(
            Request {
                source:
                    "function(a) { ext: std.extVar('plain'), code: std.extVar('code'), tla: a }"
                        .into(),
                ext_vars: BTreeMap::from([("plain".into(), "value".into())]),
                ext_code: BTreeMap::from([("code".into(), "{nested: true}".into())]),
                tla_code: BTreeMap::from([("a".into(), "20 + 2".into())]),
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("evaluate variables");
    assert_eq!(
        json_output(&result.output),
        json!({"ext": "value", "code": {"nested": true}, "tla": 22})
    );

    let multi = engine
        .evaluate(
            Request {
                source: "{['a.json']: {value: 1}, ['b.json']: {value: 2}}".into(),
                output_mode: OutputMode::Multi,
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("evaluate multi output");
    let files = multi.files.expect("multi files");
    assert_eq!(json_output(&files["a.json"]), json!({"value": 1}));
    assert_eq!(json_output(&files["b.json"]), json!({"value": 2}));

    let stream = engine
        .evaluate(
            Request {
                source: "[{value: 1}, {value: 2}]".into(),
                output_mode: OutputMode::Stream,
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("evaluate stream output");
    let documents = stream.documents.expect("stream documents");
    assert_eq!(json_output(&documents[0]), json!({"value": 1}));
    assert_eq!(json_output(&documents[1]), json!({"value": 2}));

    let error = engine
        .evaluate(
            Request {
                source: "error 'nope'".into(),
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect_err("Jsonnet error must fail");
    assert!(matches!(error, Error::Evaluation(_)));
}

#[test]
fn preserves_import_capability_and_trace_policy() {
    let importer = MapImporter::new(BTreeMap::from([(
        "lib/value.jsonnet".into(),
        b"21".to_vec(),
    )]))
    .expect("map importer");
    let result = engine()
        .evaluate(
            Request {
                filename: "apps/main.jsonnet".into(),
                source: "std.trace(std.repeat('x', 256), std.native('double')(import '../lib/value.jsonnet'))"
                    .into(),
                importer: Some(Arc::new(importer)),
                capabilities: BTreeMap::from([(
                    "double".into(),
                    Capability::new(vec!["value".into()], |_context, arguments| {
                        Ok(json!(arguments[0].as_i64().expect("integer") * 2))
                    }),
                )]),
                limits: RequestLimits {
                    max_trace_bytes: 64,
                    ..RequestLimits::default()
                },
                capture_trace: true,
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("evaluate host callbacks");

    assert_eq!(json_output(&result.output), json!(42));
    assert_eq!(result.trace.len(), 64);
    assert!(result.stats.trace_truncated);
    assert_eq!(result.imports, ["lib/value.jsonnet"]);
    assert_eq!(result.stats.import_resolutions, 1);
    assert_eq!(result.stats.import_bytes, 2);
    assert_eq!(result.stats.capability_calls, 1);
}

#[test]
fn enforces_host_and_guest_limits() {
    let engine = engine();
    let denied = engine
        .evaluate(
            Request {
                source: "import 'missing.jsonnet'".into(),
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect_err("ungranted import must fail");
    assert!(matches!(denied, Error::ImportDenied { .. }));

    let output_limit = engine
        .evaluate(
            Request {
                source: "'this output is too large'".into(),
                limits: RequestLimits {
                    max_output_bytes: 8,
                    ..RequestLimits::default()
                },
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect_err("output limit must fail");
    assert!(matches!(output_limit, Error::Limit { .. }));

    let first = engine
        .evaluate(
            Request {
                source: "{answer: 6 * 7}".into(),
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("measure fuel");
    let exact = first.stats.fuel_consumed;
    assert!(exact > 1);
    let second = engine
        .evaluate(
            Request {
                source: "{answer: 6 * 7}".into(),
                limits: RequestLimits {
                    max_fuel: exact,
                    ..RequestLimits::default()
                },
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect("exact deterministic fuel");
    assert_eq!(second.stats.fuel_consumed, exact);

    let fuel_limit = engine
        .evaluate(
            Request {
                source: "{answer: 6 * 7}".into(),
                limits: RequestLimits {
                    max_fuel: exact / 2,
                    ..RequestLimits::default()
                },
                ..Request::default()
            },
            TIMEOUT,
        )
        .expect_err("reduced fuel budget must fail");
    assert!(matches!(
        fuel_limit,
        Error::Limit {
            resource: "fuel",
            ..
        }
    ));
}

#[test]
fn epoch_deadline_interrupts_guest_execution() {
    let error = engine()
        .evaluate(
            Request {
                source: "std.foldl(function(x, y) x + y, std.range(1, 2000000), 0)".into(),
                ..Request::default()
            },
            Duration::from_millis(20),
        )
        .expect_err("deadline must interrupt execution");
    assert!(matches!(error, Error::Canceled), "{error:?}");
}
