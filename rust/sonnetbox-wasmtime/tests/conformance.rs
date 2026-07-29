use std::collections::BTreeMap;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use serde::Deserialize;
use serde_json::{Value, json};
use sonnetbox_wasmtime::{
    Capability, Engine, EngineConfig, Error, FUEL_MODEL, MapImporter, OutputMode, Request,
    RequestLimits,
};

const GUEST: &[u8] = include_bytes!("../../../guest/sonnetbox.wasm");
const SHARED_CONFORMANCE: &str = include_str!("../../../abi/v7/conformance.json");
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

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SharedSuite {
    cases: Vec<SharedCase>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SharedCase {
    name: String,
    input_mode: String,
    request: SharedRequest,
    #[serde(default)]
    imports: BTreeMap<String, String>,
    #[serde(default)]
    capabilities: BTreeMap<String, SharedCapability>,
    expected: SharedExpected,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct SharedRequest {
    filename: String,
    source: String,
    ext_vars: BTreeMap<String, String>,
    output_mode: String,
    capture_trace: bool,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct SharedCapability {
    params: Vec<String>,
    args: Vec<Value>,
    result: Value,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct SharedExpected {
    output: Option<Value>,
    files: Option<BTreeMap<String, Value>>,
    documents: Option<Vec<Value>>,
    imports: Vec<String>,
    trace_contains: String,
}

#[test]
fn runs_shared_go_and_rust_conformance_cases() {
    let suite: SharedSuite =
        serde_json::from_str(SHARED_CONFORMANCE).expect("decode shared conformance suite");
    assert!(!suite.cases.is_empty());

    for test_case in suite.cases {
        let importer = if test_case.imports.is_empty() {
            None
        } else {
            let files = test_case
                .imports
                .into_iter()
                .map(|(name, content)| (name, content.into_bytes()))
                .collect();
            Some(
                Arc::new(MapImporter::new(files).expect("shared map importer"))
                    as Arc<dyn sonnetbox_wasmtime::Importer>,
            )
        };
        let capabilities = test_case
            .capabilities
            .into_iter()
            .map(|(name, declaration)| {
                let expected_args = declaration.args;
                let result = declaration.result;
                (
                    name,
                    Capability::new(declaration.params, move |_context, arguments| {
                        if arguments != expected_args {
                            return Err(format!(
                                "arguments {arguments:?} do not match fixture {expected_args:?}"
                            ));
                        }
                        Ok(result.clone())
                    }),
                )
            })
            .collect();
        let output_mode = match test_case.request.output_mode.as_str() {
            "" | "single" => OutputMode::Single,
            "multi" => OutputMode::Multi,
            "stream" => OutputMode::Stream,
            mode => panic!("unknown shared output mode {mode:?}"),
        };
        let request = Request {
            filename: test_case.request.filename.clone(),
            source: test_case.request.source,
            ext_vars: test_case.request.ext_vars,
            importer,
            capabilities,
            output_mode,
            capture_trace: test_case.request.capture_trace,
            ..Request::default()
        };
        let result = match test_case.input_mode.as_str() {
            "snippet" => engine().evaluate(request, TIMEOUT),
            "anonymous" => engine().evaluate_anonymous(request, TIMEOUT),
            "file" => engine().evaluate_file(test_case.request.filename, request, TIMEOUT),
            mode => panic!("unknown shared input mode {mode:?}"),
        }
        .unwrap_or_else(|error| panic!("shared case {:?}: {error}", test_case.name));

        if let Some(expected) = test_case.expected.output {
            assert_eq!(
                json_output(&result.output),
                expected,
                "shared case {:?}",
                test_case.name
            );
        }
        if let Some(expected_files) = test_case.expected.files {
            let files = result.files.expect("shared multi-file output");
            assert_eq!(files.len(), expected_files.len());
            for (name, expected) in expected_files {
                assert_eq!(
                    json_output(&files[&name]),
                    expected,
                    "shared case {:?} file {name:?}",
                    test_case.name
                );
            }
        }
        if let Some(expected_documents) = test_case.expected.documents {
            let documents = result.documents.expect("shared stream output");
            assert_eq!(documents.len(), expected_documents.len());
            for (index, expected) in expected_documents.into_iter().enumerate() {
                assert_eq!(
                    json_output(&documents[index]),
                    expected,
                    "shared case {:?} document {index}",
                    test_case.name
                );
            }
        }
        assert_eq!(
            result.imports, test_case.expected.imports,
            "shared case {:?}",
            test_case.name
        );
        if !test_case.expected.trace_contains.is_empty() {
            assert!(
                String::from_utf8_lossy(&result.trace).contains(&test_case.expected.trace_contains),
                "shared case {:?} trace {:?}",
                test_case.name,
                result.trace
            );
        }
        assert!(result.stats.fuel_consumed > 0);
    }
}
