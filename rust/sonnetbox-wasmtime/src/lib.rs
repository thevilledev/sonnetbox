//! A Wasmtime implementation of the public sonnetbox ABI.
//!
//! The host compiles one guest module and creates a fresh WASI store and
//! instance for every evaluation. WASI starts with no inherited environment,
//! arguments, directories, network, or standard streams.

mod config;
mod error;
mod protocol;

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Component, Path};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex, mpsc};
use std::thread;
use std::time::{Duration, Instant};

pub use config::{EngineConfig, RequestLimits};
pub use error::Error;
use protocol::{
    ABI_VERSION, CapabilityDescriptor, CapabilityRequest, CapabilityResponse, EVAL_HOST_ERROR,
    EVAL_INTERNAL, EVAL_INVALID_REQUEST, EVAL_JSONNET_ERROR, EVAL_LIMIT, EVAL_OK,
    EvaluationRequest, GuestError, GuestLimits, HOST_CANCELED, HOST_DENIED, HOST_HANDLER_FAILURE,
    HOST_LIMIT, HOST_MALFORMED, HOST_OK, ImportRequest, ImportResponse, InputMode,
    OP_CALL_CAPABILITY, OP_RESOLVE_IMPORT,
};
use serde::de::DeserializeOwned;
use serde_json::Value;
use wasmtime::{
    Caller, Config, Engine as WasmtimeEngine, ExternType, Instance, InstancePre, Linker, Memory,
    Module, Store, StoreLimits, StoreLimitsBuilder, Trap, ValType,
};
use wasmtime_wasi::WasiCtxBuilder;
use wasmtime_wasi::p1::{self, WasiP1Ctx};
use wasmtime_wasi::{Deterministic, HostMonotonicClock, HostWallClock};

/// The deterministic fuel schedule used by this host.
pub const FUEL_MODEL: &str = "wasmtime-fuel-v1";
/// The ABI implemented by this host.
pub const HOST_ABI_VERSION: u32 = ABI_VERSION;
/// The embedded evaluator version declared by ABI 7.
pub const JSONNET_VERSION: &str = "v0.22.0";

const EPOCH_TICK: Duration = Duration::from_millis(10);

/// Selects how a top-level Jsonnet value is manifested.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
#[repr(u8)]
pub enum OutputMode {
    #[default]
    Single = 0,
    Multi = 1,
    Stream = 2,
}

/// Context passed to trusted host callbacks.
#[derive(Clone, Copy, Debug)]
pub struct HostContext {
    deadline: Instant,
}

impl HostContext {
    pub fn deadline(self) -> Instant {
        self.deadline
    }

    pub fn is_expired(self) -> bool {
        Instant::now() >= self.deadline
    }
}

/// Result of resolving one virtual import.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Import {
    pub canonical: String,
    pub content: Vec<u8>,
}

/// A trusted importer failure. `Denied` is a policy or not-found result.
#[derive(Clone, Debug, thiserror::Error)]
pub enum ImportFailure {
    #[error("{0}")]
    Denied(String),
    #[error("{0}")]
    Failed(String),
}

/// Resolves Jsonnet imports without exposing a guest filesystem.
pub trait Importer: Send + Sync {
    fn import(
        &self,
        context: HostContext,
        imported_from: &str,
        imported_path: &str,
    ) -> std::result::Result<Import, ImportFailure>;
}

/// An immutable importer backed by canonical virtual paths.
#[derive(Clone, Debug)]
pub struct MapImporter {
    files: BTreeMap<String, Vec<u8>>,
}

impl MapImporter {
    pub fn new(files: BTreeMap<String, Vec<u8>>) -> std::result::Result<Self, Error> {
        for name in files.keys() {
            validate_virtual_path(name).map_err(|message| Error::InvalidRequest {
                field: "import path",
                message,
            })?;
        }
        Ok(Self { files })
    }
}

impl Importer for MapImporter {
    fn import(
        &self,
        context: HostContext,
        imported_from: &str,
        imported_path: &str,
    ) -> std::result::Result<Import, ImportFailure> {
        if context.is_expired() {
            return Err(ImportFailure::Failed("deadline exceeded".into()));
        }
        let canonical =
            resolve_virtual_import(imported_from, imported_path).map_err(ImportFailure::Denied)?;
        let content = self
            .files
            .get(&canonical)
            .ok_or_else(|| ImportFailure::Denied(format!("{canonical:?} is missing")))?
            .clone();
        Ok(Import { canonical, content })
    }
}

type CapabilityCall =
    dyn Fn(HostContext, &[Value]) -> std::result::Result<Value, String> + Send + Sync + 'static;

/// A pure, explicitly granted Jsonnet native function.
#[derive(Clone)]
pub struct Capability {
    pub params: Vec<String>,
    call: Arc<CapabilityCall>,
}

impl Capability {
    pub fn new<F>(params: Vec<String>, call: F) -> Self
    where
        F: Fn(HostContext, &[Value]) -> std::result::Result<Value, String> + Send + Sync + 'static,
    {
        Self {
            params,
            call: Arc::new(call),
        }
    }
}

/// One isolated evaluation request.
pub struct Request {
    pub filename: String,
    pub source: String,
    pub ext_vars: BTreeMap<String, String>,
    pub ext_code: BTreeMap<String, String>,
    pub tla_vars: BTreeMap<String, String>,
    pub tla_code: BTreeMap<String, String>,
    pub importer: Option<Arc<dyn Importer>>,
    pub capabilities: BTreeMap<String, Capability>,
    pub limits: RequestLimits,
    pub output_mode: OutputMode,
    pub string_output: bool,
    pub omit_trailing_newline: bool,
    pub capture_trace: bool,
}

impl Default for Request {
    fn default() -> Self {
        Self {
            filename: String::new(),
            source: String::new(),
            ext_vars: BTreeMap::new(),
            ext_code: BTreeMap::new(),
            tla_vars: BTreeMap::new(),
            tla_code: BTreeMap::new(),
            importer: None,
            capabilities: BTreeMap::new(),
            limits: RequestLimits::default(),
            output_mode: OutputMode::Single,
            string_output: false,
            omit_trailing_newline: false,
            capture_trace: false,
        }
    }
}

/// Bounded work observed during an evaluation.
#[derive(Clone, Debug, Default)]
pub struct EvaluationStats {
    pub queue_duration: Duration,
    pub execution_duration: Duration,
    pub fuel_consumed: u64,
    pub fuel_model: &'static str,
    pub import_resolutions: u32,
    pub import_bytes: u64,
    pub capability_calls: u32,
    pub trace_bytes: u32,
    pub trace_truncated: bool,
}

/// A completed Jsonnet evaluation.
#[derive(Clone, Debug, Default)]
pub struct Result {
    pub output: Vec<u8>,
    pub files: Option<BTreeMap<String, Vec<u8>>>,
    pub documents: Option<Vec<Vec<u8>>>,
    pub trace: Vec<u8>,
    pub imports: Vec<String>,
    pub stats: EvaluationStats,
}

/// A compiled, concurrently usable Wasmtime host.
#[derive(Clone)]
pub struct Engine {
    inner: Arc<EngineInner>,
}

struct EngineInner {
    engine: WasmtimeEngine,
    pre: InstancePre<StoreState>,
    config: EngineConfig,
    gate: Gate,
    epoch_stop: mpsc::Sender<()>,
    epoch_thread: Mutex<Option<thread::JoinHandle<()>>>,
}

impl Drop for EngineInner {
    fn drop(&mut self) {
        let _ = self.epoch_stop.send(());
        if let Some(handle) = self
            .epoch_thread
            .lock()
            .expect("epoch thread lock poisoned")
            .take()
        {
            let _ = handle.join();
        }
    }
}

struct StoreState {
    wasi: WasiP1Ctx,
    store_limits: StoreLimits,
    invocation: InvocationState,
}

struct InvocationState {
    context: HostContext,
    limits: RequestLimits,
    importer: Option<Arc<dyn Importer>>,
    capabilities: BTreeMap<String, Capability>,
    import_calls: u32,
    import_bytes: u64,
    capability_calls: u32,
    resolved: Vec<String>,
    resolved_seen: BTreeSet<String>,
    last_error: Option<Error>,
}

impl InvocationState {
    fn empty(deadline: Instant, limits: RequestLimits) -> Self {
        Self {
            context: HostContext { deadline },
            limits,
            importer: None,
            capabilities: BTreeMap::new(),
            import_calls: 0,
            import_bytes: 0,
            capability_calls: 0,
            resolved: Vec::new(),
            resolved_seen: BTreeSet::new(),
            last_error: None,
        }
    }

    fn record(&mut self, error: Error) {
        if self.last_error.is_none() {
            self.last_error = Some(error);
        }
    }
}

impl Engine {
    /// Compiles and validates a sonnetbox guest.
    pub fn new(guest: &[u8], config: EngineConfig) -> std::result::Result<Self, Error> {
        let config = config.validate()?;
        let mut runtime_config = Config::new();
        runtime_config.consume_fuel(true);
        runtime_config.epoch_interruption(true);
        let max_wasm_stack = usize::try_from(config.max_wasm_stack_bytes)
            .map_err(|_| Error::invalid("max_wasm_stack_bytes", "does not fit usize"))?;
        runtime_config.max_wasm_stack(max_wasm_stack);
        runtime_config.async_stack_size(max_wasm_stack.saturating_add(2 << 20));
        let runtime = WasmtimeEngine::new(&runtime_config)
            .map_err(|error| Error::abi(format!("create Wasmtime engine: {error}")))?;
        let module = Module::new(&runtime, guest)
            .map_err(|error| Error::abi(format!("compile guest: {error}")))?;
        validate_module(&module)?;

        let mut linker = Linker::<StoreState>::new(&runtime);
        p1::add_to_linker_sync(&mut linker, |state| &mut state.wasi)
            .map_err(|error| Error::abi(format!("link WASI Preview 1: {error}")))?;
        linker
            .func_wrap("sonnetbox_host", "call", host_call)
            .map_err(|error| Error::abi(format!("link sonnetbox_host.call: {error}")))?;
        let pre = linker
            .instantiate_pre(&module)
            .map_err(|error| Error::abi(format!("prepare guest instantiation: {error}")))?;

        let (epoch_stop, epoch_rx) = mpsc::channel();
        let epoch_engine = runtime.clone();
        let epoch_thread = thread::Builder::new()
            .name("sonnetbox-wasmtime-epoch".into())
            .spawn(move || {
                while epoch_rx.recv_timeout(EPOCH_TICK).is_err() {
                    epoch_engine.increment_epoch();
                }
            })
            .map_err(|error| Error::abi(format!("start deadline ticker: {error}")))?;

        let engine = Self {
            inner: Arc::new(EngineInner {
                engine: runtime,
                pre,
                config,
                gate: Gate::new(config.max_concurrent_evaluations),
                epoch_stop,
                epoch_thread: Mutex::new(Some(epoch_thread)),
            }),
        };
        engine.probe()?;
        Ok(engine)
    }

    pub fn config(&self) -> EngineConfig {
        self.inner.config
    }

    /// Evaluates an inline snippet with imports relative to its filename.
    pub fn evaluate(
        &self,
        request: Request,
        timeout: Duration,
    ) -> std::result::Result<Result, Error> {
        self.evaluate_mode(request, InputMode::Snippet, timeout)
    }

    /// Evaluates an inline snippet whose imports resolve from the virtual root.
    pub fn evaluate_anonymous(
        &self,
        request: Request,
        timeout: Duration,
    ) -> std::result::Result<Result, Error> {
        self.evaluate_mode(request, InputMode::Anonymous, timeout)
    }

    /// Loads and evaluates a canonical filename through the request importer.
    pub fn evaluate_file(
        &self,
        filename: impl Into<String>,
        mut request: Request,
        timeout: Duration,
    ) -> std::result::Result<Result, Error> {
        if !request.source.is_empty() {
            return Err(Error::invalid(
                "source",
                "must be empty for file evaluation",
            ));
        }
        let filename = filename.into();
        if !request.filename.is_empty() && request.filename != filename {
            return Err(Error::invalid(
                "filename",
                "conflicts with evaluate_file filename",
            ));
        }
        request.filename = filename;
        self.evaluate_mode(request, InputMode::File, timeout)
    }

    fn probe(&self) -> std::result::Result<(), Error> {
        let deadline = Instant::now() + Duration::from_secs(30);
        let limits = RequestLimits::default().resolve(self.inner.config)?;
        let (mut store, instance) =
            self.instantiate(InvocationState::empty(deadline, limits), u64::MAX, deadline)?;
        let version = call_u32(
            &mut store,
            &instance,
            "sonnetbox_abi_version",
            "ABI probe",
            RequestLimits {
                max_fuel: u64::MAX,
                ..RequestLimits::default()
            },
            self.inner.config,
        )?;
        if version != ABI_VERSION {
            return Err(Error::abi(format!(
                "guest ABI version {version} does not match host version {ABI_VERSION}"
            )));
        }
        Ok(())
    }

    fn evaluate_mode(
        &self,
        mut request: Request,
        input_mode: InputMode,
        timeout: Duration,
    ) -> std::result::Result<Result, Error> {
        if timeout.is_zero() {
            return Err(Error::invalid("timeout", "must be positive"));
        }
        let started = Instant::now();
        let deadline = started
            .checked_add(timeout)
            .ok_or_else(|| Error::invalid("timeout", "deadline overflow"))?;
        prepare_request(&mut request, input_mode, self.inner.config)?;
        let limits = request.limits;
        let encoded = encode_request(&request, input_mode)?;
        if encoded.len() as u64 > u64::from(limits.max_host_request_bytes) {
            return Err(Error::Limit {
                resource: "encoded request bytes",
                limit: u64::from(limits.max_host_request_bytes),
                actual: encoded.len() as u64,
            });
        }
        let queue_started = Instant::now();
        let permit = self.inner.gate.acquire(deadline)?;
        let queue_duration = queue_started.elapsed();
        let execution_started = Instant::now();
        if Instant::now() >= deadline {
            return Err(Error::Canceled);
        }

        let invocation = InvocationState {
            context: HostContext { deadline },
            limits,
            importer: request.importer,
            capabilities: request.capabilities,
            import_calls: 0,
            import_bytes: 0,
            capability_calls: 0,
            resolved: Vec::new(),
            resolved_seen: BTreeSet::new(),
            last_error: None,
        };
        let (mut store, instance) = self.instantiate(invocation, limits.max_fuel, deadline)?;
        let memory = instance
            .get_memory(&mut store, "memory")
            .ok_or_else(|| Error::abi("missing memory export"))?;
        let request_length = i32::try_from(encoded.len())
            .map_err(|_| Error::invalid("request", "encoded request is too large"))?;
        let alloc = instance
            .get_typed_func::<i32, i32>(&mut store, "sonnetbox_request_alloc")
            .map_err(|error| Error::abi(format!("request allocator signature: {error}")))?;
        let request_ptr = alloc.call(&mut store, request_length).map_err(|error| {
            runtime_error(
                error.into(),
                "request allocation",
                limits,
                self.inner.config,
            )
        })?;
        let request_offset = positive_offset(request_ptr, "request pointer")?;
        memory
            .write(&mut store, request_offset, &encoded)
            .map_err(|error| Error::abi(format!("write evaluation request: {error}")))?;

        let evaluate = instance
            .get_typed_func::<(), i32>(&mut store, "sonnetbox_evaluate")
            .map_err(|error| Error::abi(format!("evaluate signature: {error}")))?;
        let status = evaluate
            .call(&mut store, ())
            .map_err(|error| runtime_error(error.into(), "evaluation", limits, self.inner.config))?
            as u32;

        let (trace, trace_truncated) = if request.capture_trace {
            read_trace(&mut store, &instance, &memory, limits, self.inner.config)?
        } else {
            (Vec::new(), false)
        };

        if status == EVAL_HOST_ERROR
            && let Some(error) = store.data_mut().invocation.last_error.take()
        {
            return Err(error);
        }
        let result_ptr = call_u32(
            &mut store,
            &instance,
            "sonnetbox_result_ptr",
            "result pointer",
            limits,
            self.inner.config,
        )?;
        let result_len = call_u32(
            &mut store,
            &instance,
            "sonnetbox_result_len",
            "result length",
            limits,
            self.inner.config,
        )?;
        let payload_limit = if status == EVAL_OK {
            limits.max_output_bytes
        } else {
            limits.max_host_response_bytes
        };
        let payload = read_guest_bytes(
            &mut store,
            &memory,
            result_ptr,
            result_len,
            payload_limit,
            "guest result",
        )?;

        let mut result = if status == EVAL_OK {
            protocol::decode_output(request.output_mode, &payload)?
        } else {
            return Err(guest_status_error(status, &payload, limits));
        };
        let remaining = store
            .get_fuel()
            .map_err(|error| Error::abi(format!("read remaining fuel: {error}")))?;
        let invocation = &store.data().invocation;
        result.trace = trace;
        result.imports = invocation.resolved.clone();
        result.stats = EvaluationStats {
            queue_duration,
            execution_duration: execution_started.elapsed(),
            fuel_consumed: limits.max_fuel.saturating_sub(remaining),
            fuel_model: FUEL_MODEL,
            import_resolutions: invocation.import_calls,
            import_bytes: invocation.import_bytes,
            capability_calls: invocation.capability_calls,
            trace_bytes: result.trace.len() as u32,
            trace_truncated,
        };
        drop(permit);
        Ok(result)
    }

    fn instantiate(
        &self,
        invocation: InvocationState,
        fuel: u64,
        deadline: Instant,
    ) -> std::result::Result<(Store<StoreState>, Instance), Error> {
        let mut wasi_builder = WasiCtxBuilder::new();
        wasi_builder
            .secure_random(Deterministic::new(vec![0x53, 0x42, 0x58, 0x07]))
            .insecure_random(Deterministic::new(vec![0x53, 0x42, 0x58, 0x07]))
            .insecure_random_seed(0x5342_5807)
            .wall_clock(FixedWallClock)
            .monotonic_clock(StepMonotonicClock::default());
        let wasi = wasi_builder.build_p1();
        let store_limits = StoreLimitsBuilder::new()
            .memory_size(self.inner.config.max_memory_bytes as usize)
            .build();
        let mut store = Store::new(
            &self.inner.engine,
            StoreState {
                wasi,
                store_limits,
                invocation,
            },
        );
        store.limiter(|state| &mut state.store_limits);
        store
            .set_fuel(fuel)
            .map_err(|error| Error::abi(format!("set evaluation fuel: {error}")))?;
        store.epoch_deadline_trap();
        store.set_epoch_deadline(deadline_ticks(deadline));
        let instance = self.inner.pre.instantiate(&mut store).map_err(|error| {
            runtime_error(
                error.into(),
                "initialization",
                RequestLimits {
                    max_fuel: fuel,
                    ..RequestLimits::default()
                },
                self.inner.config,
            )
        })?;
        let initialize = instance
            .get_typed_func::<(), ()>(&mut store, "_initialize")
            .map_err(|error| Error::abi(format!("initialize signature: {error}")))?;
        initialize.call(&mut store, ()).map_err(|error| {
            runtime_error(
                error.into(),
                "initialization",
                RequestLimits {
                    max_fuel: fuel,
                    ..RequestLimits::default()
                },
                self.inner.config,
            )
        })?;
        Ok((store, instance))
    }
}

fn prepare_request(
    request: &mut Request,
    input_mode: InputMode,
    config: EngineConfig,
) -> std::result::Result<(), Error> {
    if request.filename.is_empty() {
        request.filename = "snippet.jsonnet".into();
    }
    validate_virtual_path(&request.filename).map_err(|message| Error::InvalidRequest {
        field: "filename",
        message,
    })?;
    if input_mode == InputMode::File && !request.source.is_empty() {
        return Err(Error::invalid(
            "source",
            "must be empty for file evaluation",
        ));
    }
    request.limits = request.limits.resolve(config)?;
    if request.source.len() as u64 > u64::from(request.limits.max_source_bytes) {
        return Err(Error::Limit {
            resource: "source bytes",
            limit: u64::from(request.limits.max_source_bytes),
            actual: request.source.len() as u64,
        });
    }
    for (name, capability) in &request.capabilities {
        if name.is_empty() {
            return Err(Error::invalid("capabilities", "name must not be empty"));
        }
        let mut seen = BTreeSet::new();
        for param in &capability.params {
            if !is_identifier(param) {
                return Err(Error::invalid(
                    "capabilities.params",
                    format!("{param:?} is not a Jsonnet identifier"),
                ));
            }
            if !seen.insert(param) {
                return Err(Error::invalid(
                    "capabilities.params",
                    format!("{param:?} is duplicated"),
                ));
            }
        }
    }
    Ok(())
}

fn encode_request(request: &Request, input_mode: InputMode) -> std::result::Result<Vec<u8>, Error> {
    let capabilities = request
        .capabilities
        .iter()
        .map(|(name, capability)| {
            (
                name.clone(),
                CapabilityDescriptor {
                    params: &capability.params,
                },
            )
        })
        .collect();
    serde_json::to_vec(&EvaluationRequest {
        filename: &request.filename,
        source: &request.source,
        input_mode: input_mode as u8,
        output_mode: request.output_mode as u8,
        ext_vars: &request.ext_vars,
        ext_code: &request.ext_code,
        tla_vars: &request.tla_vars,
        tla_code: &request.tla_code,
        capabilities,
        string_output: request.string_output,
        output_newline: !request.omit_trailing_newline,
        capture_trace: request.capture_trace,
        limits: GuestLimits::from(request.limits),
    })
    .map_err(|error| Error::invalid("request", format!("encode request: {error}")))
}

fn host_call(
    mut caller: Caller<'_, StoreState>,
    operation: i32,
    request_ptr: i32,
    request_len: i32,
    response_ptr: i32,
    response_capacity: i32,
) -> i64 {
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        host_call_inner(
            &mut caller,
            operation as u32,
            request_ptr,
            request_len,
            response_ptr,
            response_capacity,
        )
    }));
    match result {
        Ok(packed) => packed,
        Err(_) => {
            let error = match operation as u32 {
                OP_RESOLVE_IMPORT => Error::Import {
                    from: String::new(),
                    path: "<unknown>".into(),
                    message: "handler panicked".into(),
                },
                OP_CALL_CAPABILITY => Error::Capability {
                    name: "<unknown>".into(),
                    message: "handler panicked".into(),
                },
                operation => Error::abi(format!("host operation {operation} panicked")),
            };
            caller.data_mut().invocation.record(error);
            protocol::pack(HOST_HANDLER_FAILURE, 0)
        }
    }
}

fn host_call_inner(
    caller: &mut Caller<'_, StoreState>,
    operation: u32,
    request_ptr: i32,
    request_len: i32,
    response_ptr: i32,
    response_capacity: i32,
) -> i64 {
    let memory = match caller
        .get_export("memory")
        .and_then(|item| item.into_memory())
    {
        Some(memory) => memory,
        None => return record_host_error(caller, Error::abi("guest memory is unavailable")),
    };
    let request_limit = caller.data().invocation.limits.max_host_request_bytes;
    let response_limit = caller.data().invocation.limits.max_host_response_bytes;
    let raw = match read_callback_request(caller, &memory, request_ptr, request_len, request_limit)
    {
        Ok(raw) => raw,
        Err(error) => return record_host_error(caller, error),
    };
    let response_offset = match bounded_range(
        caller,
        &memory,
        response_ptr,
        response_capacity,
        response_limit,
        "host response",
    ) {
        Ok(offset) => offset,
        Err(error) => return record_host_error(caller, error),
    };

    let (status, payload, error) = match operation {
        OP_RESOLVE_IMPORT => resolve_import(caller.data_mut(), &raw),
        OP_CALL_CAPABILITY => call_capability(caller.data_mut(), &raw),
        _ => {
            let error = Error::abi(format!("unknown host operation {operation}"));
            (HOST_MALFORMED, error.to_string().into_bytes(), Some(error))
        }
    };
    if let Some(error) = error {
        caller.data_mut().invocation.record(error);
    }
    let capacity = response_capacity as usize;
    if status == HOST_OK && payload.len() > capacity {
        let error = Error::Limit {
            resource: "host response capacity",
            limit: capacity as u64,
            actual: payload.len() as u64,
        };
        caller.data_mut().invocation.record(error);
        return protocol::pack(HOST_LIMIT, 0);
    }
    let payload = if status == HOST_OK {
        payload
    } else {
        utf8_prefix(payload, capacity)
    };
    let written = payload.len().min(capacity);
    if let Err(error) = memory.write(&mut *caller, response_offset, &payload[..written]) {
        caller
            .data_mut()
            .invocation
            .record(Error::abi(format!("write host response: {error}")));
        return protocol::pack(HOST_MALFORMED, 0);
    }
    protocol::pack(status, written)
}

fn resolve_import(state: &mut StoreState, raw: &[u8]) -> (u32, Vec<u8>, Option<Error>) {
    let request: ImportRequest = match decode_strict(raw) {
        Ok(request) => request,
        Err(message) => {
            let error = Error::abi(format!("decode import request: {message}"));
            return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
        }
    };
    if request.imported_path.is_empty() {
        let error = Error::abi("import request path is empty");
        return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
    }
    if !request.imported_from.is_empty()
        && let Err(message) = validate_virtual_path(&request.imported_from)
    {
        let error = Error::abi(format!("invalid importing path: {message}"));
        return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
    }
    if let Err(message) = validate_import_path(&request.imported_path) {
        let error = Error::ImportDenied {
            from: request.imported_from,
            path: request.imported_path,
            message,
        };
        return (HOST_DENIED, error.to_string().into_bytes(), Some(error));
    }
    let invocation = &mut state.invocation;
    if invocation.import_calls >= invocation.limits.max_imports {
        let error = Error::Limit {
            resource: "import count",
            limit: u64::from(invocation.limits.max_imports),
            actual: u64::from(invocation.import_calls) + 1,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    invocation.import_calls += 1;
    if invocation.context.is_expired() {
        let error = Error::Canceled;
        return (HOST_CANCELED, error.to_string().into_bytes(), Some(error));
    }
    let Some(importer) = invocation.importer.as_ref() else {
        let error = Error::ImportDenied {
            from: request.imported_from,
            path: request.imported_path,
            message: "no importer was granted".into(),
        };
        return (HOST_DENIED, error.to_string().into_bytes(), Some(error));
    };
    let imported = match importer.import(
        invocation.context,
        &request.imported_from,
        &request.imported_path,
    ) {
        Ok(imported) => imported,
        Err(ImportFailure::Denied(message)) => {
            let error = Error::ImportDenied {
                from: request.imported_from,
                path: request.imported_path,
                message,
            };
            return (HOST_DENIED, error.to_string().into_bytes(), Some(error));
        }
        Err(ImportFailure::Failed(message)) => {
            let error = Error::Import {
                from: request.imported_from,
                path: request.imported_path,
                message,
            };
            return (
                HOST_HANDLER_FAILURE,
                error.to_string().into_bytes(),
                Some(error),
            );
        }
    };
    if invocation.context.is_expired() {
        let error = Error::Canceled;
        return (HOST_CANCELED, error.to_string().into_bytes(), Some(error));
    }
    if let Err(message) = validate_virtual_path(&imported.canonical) {
        let error = Error::Import {
            from: request.imported_from,
            path: request.imported_path,
            message: format!("invalid canonical path {:?}: {message}", imported.canonical),
        };
        return (
            HOST_HANDLER_FAILURE,
            error.to_string().into_bytes(),
            Some(error),
        );
    }
    if imported.content.len() as u64 > u64::from(invocation.limits.max_import_bytes) {
        let error = Error::Limit {
            resource: "import bytes",
            limit: u64::from(invocation.limits.max_import_bytes),
            actual: imported.content.len() as u64,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    let total = invocation
        .import_bytes
        .saturating_add(imported.content.len() as u64);
    if total > invocation.limits.max_total_import_bytes {
        let error = Error::Limit {
            resource: "total import bytes",
            limit: invocation.limits.max_total_import_bytes,
            actual: total,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    invocation.import_bytes = total;
    if invocation.resolved_seen.insert(imported.canonical.clone()) {
        invocation.resolved.push(imported.canonical.clone());
    }
    let encoded =
        match serde_json::to_vec(&ImportResponse::new(&imported.canonical, &imported.content)) {
            Ok(encoded) => encoded,
            Err(error) => {
                let error = Error::Import {
                    from: request.imported_from,
                    path: request.imported_path,
                    message: format!("encode response: {error}"),
                };
                return (
                    HOST_HANDLER_FAILURE,
                    error.to_string().into_bytes(),
                    Some(error),
                );
            }
        };
    if encoded.len() as u64 > u64::from(invocation.limits.max_host_response_bytes) {
        let error = Error::Limit {
            resource: "import response bytes",
            limit: u64::from(invocation.limits.max_host_response_bytes),
            actual: encoded.len() as u64,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    (HOST_OK, encoded, None)
}

fn call_capability(state: &mut StoreState, raw: &[u8]) -> (u32, Vec<u8>, Option<Error>) {
    let request: CapabilityRequest = match decode_strict(raw) {
        Ok(request) => request,
        Err(message) => {
            let error = Error::abi(format!("decode capability request: {message}"));
            return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
        }
    };
    if request.name.is_empty() {
        let error = Error::abi("capability request name is empty");
        return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
    }
    let invocation = &mut state.invocation;
    if invocation.capability_calls >= invocation.limits.max_capability_calls {
        let error = Error::Limit {
            resource: "capability calls",
            limit: u64::from(invocation.limits.max_capability_calls),
            actual: u64::from(invocation.capability_calls) + 1,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    invocation.capability_calls += 1;
    if invocation.context.is_expired() {
        let error = Error::Canceled;
        return (HOST_CANCELED, error.to_string().into_bytes(), Some(error));
    }
    let Some(capability) = invocation.capabilities.get(&request.name) else {
        let error = Error::Capability {
            name: request.name,
            message: "capability is not registered".into(),
        };
        return (HOST_DENIED, error.to_string().into_bytes(), Some(error));
    };
    if request.args.len() != capability.params.len() {
        let error = Error::Capability {
            name: request.name,
            message: format!(
                "guest passed {} arguments, want {}",
                request.args.len(),
                capability.params.len()
            ),
        };
        return (HOST_MALFORMED, error.to_string().into_bytes(), Some(error));
    }
    let value = match (capability.call)(invocation.context, &request.args) {
        Ok(value) => value,
        Err(message) => {
            let error = Error::Capability {
                name: request.name,
                message,
            };
            return (
                HOST_HANDLER_FAILURE,
                error.to_string().into_bytes(),
                Some(error),
            );
        }
    };
    if invocation.context.is_expired() {
        let error = Error::Canceled;
        return (HOST_CANCELED, error.to_string().into_bytes(), Some(error));
    }
    let encoded = match serde_json::to_vec(&CapabilityResponse { value }) {
        Ok(encoded) => encoded,
        Err(error) => {
            let error = Error::Capability {
                name: request.name,
                message: format!("encode result: {error}"),
            };
            return (
                HOST_HANDLER_FAILURE,
                error.to_string().into_bytes(),
                Some(error),
            );
        }
    };
    if encoded.len() as u64 > u64::from(invocation.limits.max_host_response_bytes) {
        let error = Error::Limit {
            resource: "capability response bytes",
            limit: u64::from(invocation.limits.max_host_response_bytes),
            actual: encoded.len() as u64,
        };
        return (HOST_LIMIT, error.to_string().into_bytes(), Some(error));
    }
    (HOST_OK, encoded, None)
}

fn record_host_error(caller: &mut Caller<'_, StoreState>, error: Error) -> i64 {
    caller.data_mut().invocation.record(error);
    protocol::pack(HOST_MALFORMED, 0)
}

fn read_callback_request(
    caller: &mut Caller<'_, StoreState>,
    memory: &Memory,
    ptr: i32,
    length: i32,
    limit: u32,
) -> std::result::Result<Vec<u8>, Error> {
    let offset = bounded_range(caller, memory, ptr, length, limit, "host request")?;
    if length == 0 {
        return Err(Error::abi("host request is empty"));
    }
    let mut raw = vec![0; length as usize];
    memory
        .read(caller, offset, &mut raw)
        .map_err(|error| Error::abi(format!("read host request: {error}")))?;
    Ok(raw)
}

fn bounded_range(
    caller: &mut Caller<'_, StoreState>,
    memory: &Memory,
    ptr: i32,
    length: i32,
    limit: u32,
    resource: &'static str,
) -> std::result::Result<usize, Error> {
    if ptr <= 0 {
        return Err(Error::abi(format!(
            "{resource} pointer is zero or negative"
        )));
    }
    if length <= 0 {
        return Err(Error::abi(format!("{resource} length is zero or negative")));
    }
    let length = length as usize;
    if length as u64 > u64::from(limit) {
        return Err(Error::Limit {
            resource,
            limit: u64::from(limit),
            actual: length as u64,
        });
    }
    let offset = ptr as usize;
    let end = offset
        .checked_add(length)
        .ok_or_else(|| Error::abi(format!("{resource} range overflows")))?;
    if end > memory.data_size(caller) {
        return Err(Error::abi(format!(
            "{resource} range is outside guest memory"
        )));
    }
    Ok(offset)
}

fn read_trace(
    store: &mut Store<StoreState>,
    instance: &Instance,
    memory: &Memory,
    limits: RequestLimits,
    config: EngineConfig,
) -> std::result::Result<(Vec<u8>, bool), Error> {
    let ptr = call_u32(
        store,
        instance,
        "sonnetbox_trace_ptr",
        "trace pointer",
        limits,
        config,
    )?;
    let length = call_u32(
        store,
        instance,
        "sonnetbox_trace_len",
        "trace length",
        limits,
        config,
    )?;
    let truncated = call_u32(
        store,
        instance,
        "sonnetbox_trace_truncated",
        "trace truncation flag",
        limits,
        config,
    )?;
    if truncated > 1 {
        return Err(Error::abi("trace truncation flag is not zero or one"));
    }
    let trace = read_guest_bytes(store, memory, ptr, length, limits.max_trace_bytes, "trace")?;
    Ok((trace, truncated == 1))
}

fn read_guest_bytes(
    store: &mut Store<StoreState>,
    memory: &Memory,
    ptr: u32,
    length: u32,
    limit: u32,
    resource: &'static str,
) -> std::result::Result<Vec<u8>, Error> {
    if length > limit {
        return Err(Error::Limit {
            resource,
            limit: u64::from(limit),
            actual: u64::from(length),
        });
    }
    if length == 0 {
        if ptr != 0 {
            return Err(Error::abi(format!(
                "empty {resource} has a nonzero pointer"
            )));
        }
        return Ok(Vec::new());
    }
    if ptr == 0 {
        return Err(Error::abi(format!(
            "nonempty {resource} has a zero pointer"
        )));
    }
    let mut bytes = vec![0; length as usize];
    memory
        .read(store, ptr as usize, &mut bytes)
        .map_err(|error| Error::abi(format!("read {resource}: {error}")))?;
    Ok(bytes)
}

fn call_u32(
    store: &mut Store<StoreState>,
    instance: &Instance,
    name: &'static str,
    operation: &'static str,
    limits: RequestLimits,
    config: EngineConfig,
) -> std::result::Result<u32, Error> {
    let function = instance
        .get_typed_func::<(), i32>(&mut *store, name)
        .map_err(|error| Error::abi(format!("{name} signature: {error}")))?;
    let value = function
        .call(&mut *store, ())
        .map_err(|error| runtime_error(error.into(), operation, limits, config))?;
    Ok(value as u32)
}

fn runtime_error(
    source: anyhow::Error,
    operation: &'static str,
    limits: RequestLimits,
    config: EngineConfig,
) -> Error {
    match source.downcast_ref::<Trap>() {
        Some(Trap::OutOfFuel) => Error::Limit {
            resource: "fuel",
            limit: limits.max_fuel,
            actual: limits.max_fuel.saturating_add(1),
        },
        Some(Trap::Interrupt) => Error::Canceled,
        _ if source.to_string().to_ascii_lowercase().contains("memory") => Error::Limit {
            resource: "WASM memory",
            limit: config.max_memory_bytes,
            actual: config.max_memory_bytes.saturating_add(1),
        },
        _ => Error::GuestTrap { operation, source },
    }
}

fn guest_status_error(status: u32, payload: &[u8], limits: RequestLimits) -> Error {
    let guest: GuestError = match decode_strict(payload) {
        Ok(guest) => guest,
        Err(message) => {
            return Error::abi(format!("decode guest error for status {status}: {message}"));
        }
    };
    match status {
        EVAL_INVALID_REQUEST => Error::invalid("guest request", guest.message),
        EVAL_JSONNET_ERROR => Error::Evaluation(guest.message),
        EVAL_HOST_ERROR => Error::abi(format!(
            "guest reported an unclassified host error: {}",
            guest.message
        )),
        EVAL_LIMIT => Error::Limit {
            resource: "guest resource",
            limit: guest.limit.unwrap_or(u64::from(limits.max_output_bytes)),
            actual: guest.actual.unwrap_or_else(|| {
                guest
                    .limit
                    .unwrap_or(u64::from(limits.max_output_bytes))
                    .saturating_add(1)
            }),
        },
        EVAL_INTERNAL => Error::GuestTrap {
            operation: "evaluation",
            source: anyhow::anyhow!("{}: {}", guest.kind, guest.message),
        },
        _ => Error::abi(format!("unknown guest status {status}: {}", guest.message)),
    }
}

fn decode_strict<T: DeserializeOwned>(input: &[u8]) -> std::result::Result<T, serde_json::Error> {
    let mut deserializer = serde_json::Deserializer::from_slice(input);
    let value = T::deserialize(&mut deserializer)?;
    deserializer.end()?;
    Ok(value)
}

fn positive_offset(value: i32, name: &'static str) -> std::result::Result<usize, Error> {
    if value <= 0 {
        return Err(Error::abi(format!("{name} is zero or negative")));
    }
    Ok(value as usize)
}

fn deadline_ticks(deadline: Instant) -> u64 {
    let remaining = deadline.saturating_duration_since(Instant::now());
    let numerator = remaining
        .as_nanos()
        .saturating_add(EPOCH_TICK.as_nanos() - 1);
    u64::try_from(numerator / EPOCH_TICK.as_nanos())
        .unwrap_or(u64::MAX)
        .max(1)
}

struct FixedWallClock;

impl HostWallClock for FixedWallClock {
    fn resolution(&self) -> Duration {
        Duration::from_nanos(1)
    }

    fn now(&self) -> Duration {
        Duration::ZERO
    }
}

#[derive(Default)]
struct StepMonotonicClock {
    nanoseconds: AtomicU64,
}

impl HostMonotonicClock for StepMonotonicClock {
    fn resolution(&self) -> u64 {
        1
    }

    fn now(&self) -> u64 {
        self.nanoseconds.fetch_add(1, Ordering::Relaxed)
    }
}

fn utf8_prefix(mut payload: Vec<u8>, capacity: usize) -> Vec<u8> {
    let text = String::from_utf8_lossy(&payload);
    payload = text.as_bytes()[..text.len().min(capacity)].to_vec();
    while std::str::from_utf8(&payload).is_err() {
        payload.pop();
    }
    payload
}

fn is_identifier(value: &str) -> bool {
    let mut chars = value.chars();
    matches!(chars.next(), Some('_' | 'a'..='z' | 'A'..='Z'))
        && chars.all(|character| matches!(character, '_' | 'a'..='z' | 'A'..='Z' | '0'..='9'))
}

fn validate_import_path(name: &str) -> std::result::Result<(), String> {
    if name.is_empty() {
        return Err("path is empty".into());
    }
    if name.contains('\0') {
        return Err("path contains NUL".into());
    }
    if name.contains('\\') {
        return Err("backslashes are not allowed".into());
    }
    if name.starts_with('/') || Path::new(name).is_absolute() {
        return Err("absolute paths are not allowed".into());
    }
    if name.as_bytes().get(1) == Some(&b':')
        && name.as_bytes().first().is_some_and(u8::is_ascii_alphabetic)
    {
        return Err("volume-qualified paths are not allowed".into());
    }
    Ok(())
}

fn validate_virtual_path(name: &str) -> std::result::Result<(), String> {
    validate_import_path(name)?;
    if Path::new(name)
        .components()
        .any(|component| !matches!(component, Component::Normal(_)))
    {
        return Err("dot, traversal, and empty segments are not allowed".into());
    }
    if name
        .split('/')
        .any(|part| part.is_empty() || part == "." || part == "..")
    {
        return Err("dot, traversal, and empty segments are not allowed".into());
    }
    Ok(())
}

fn resolve_virtual_import(
    imported_from: &str,
    imported_path: &str,
) -> std::result::Result<String, String> {
    if !imported_from.is_empty() {
        validate_virtual_path(imported_from)?;
    }
    validate_import_path(imported_path)?;
    let mut parts: Vec<String> = imported_from
        .rsplit_once('/')
        .map_or_else(Vec::new, |(directory, _)| {
            directory.split('/').map(str::to_owned).collect()
        });
    for part in imported_path.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if parts.pop().is_none() {
                    return Err("path escapes the virtual root".into());
                }
            }
            normal => parts.push(normal.to_owned()),
        }
    }
    if parts.is_empty() {
        return Err("path escapes the virtual root".into());
    }
    let canonical = parts.join("/");
    validate_virtual_path(&canonical)?;
    Ok(canonical)
}

struct Gate {
    state: Mutex<u32>,
    available: Condvar,
    maximum: u32,
}

impl Gate {
    fn new(maximum: u32) -> Self {
        Self {
            state: Mutex::new(0),
            available: Condvar::new(),
            maximum,
        }
    }

    fn acquire(&self, deadline: Instant) -> std::result::Result<Permit<'_>, Error> {
        let mut active = self.state.lock().expect("evaluation gate poisoned");
        loop {
            if *active < self.maximum {
                *active += 1;
                return Ok(Permit { gate: self });
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Canceled);
            }
            let (next, timeout) = self
                .available
                .wait_timeout(active, remaining)
                .expect("evaluation gate poisoned");
            active = next;
            if timeout.timed_out() && *active >= self.maximum {
                return Err(Error::Canceled);
            }
        }
    }
}

struct Permit<'a> {
    gate: &'a Gate,
}

impl Drop for Permit<'_> {
    fn drop(&mut self) {
        let mut active = self.gate.state.lock().expect("evaluation gate poisoned");
        *active -= 1;
        self.gate.available.notify_one();
    }
}

fn validate_module(module: &Module) -> std::result::Result<(), Error> {
    const ALLOWED_WASI: &[&str] = &[
        "args_get",
        "args_sizes_get",
        "clock_time_get",
        "environ_get",
        "environ_sizes_get",
        "fd_close",
        "fd_fdstat_get",
        "fd_fdstat_set_flags",
        "fd_filestat_get",
        "fd_prestat_dir_name",
        "fd_prestat_get",
        "fd_read",
        "fd_write",
        "path_filestat_get",
        "path_open",
        "poll_oneoff",
        "proc_exit",
        "random_get",
        "sched_yield",
    ];
    let mut host_calls = 0;
    for import in module.imports() {
        let name = import.name();
        match import.module() {
            "wasi_snapshot_preview1" if ALLOWED_WASI.contains(&name) => {}
            "wasi_snapshot_preview1" => {
                return Err(Error::abi(format!("unexpected WASI import {name:?}")));
            }
            "sonnetbox_host" if name == "call" => {
                host_calls += 1;
                expect_signature(
                    import.ty(),
                    &[
                        ValType::I32,
                        ValType::I32,
                        ValType::I32,
                        ValType::I32,
                        ValType::I32,
                    ],
                    &[ValType::I64],
                    "sonnetbox_host.call",
                )?;
            }
            module => {
                return Err(Error::abi(format!("unexpected import {module}.{name}")));
            }
        }
    }
    if host_calls != 1 {
        return Err(Error::abi(format!(
            "guest imports sonnetbox_host.call {host_calls} times, want 1"
        )));
    }
    let expected: &[(&str, &[ValType], &[ValType])] = &[
        ("_initialize", &[], &[]),
        ("sonnetbox_abi_version", &[], &[ValType::I32]),
        ("sonnetbox_request_alloc", &[ValType::I32], &[ValType::I32]),
        ("sonnetbox_evaluate", &[], &[ValType::I32]),
        ("sonnetbox_result_ptr", &[], &[ValType::I32]),
        ("sonnetbox_result_len", &[], &[ValType::I32]),
        ("sonnetbox_trace_ptr", &[], &[ValType::I32]),
        ("sonnetbox_trace_len", &[], &[ValType::I32]),
        ("sonnetbox_trace_truncated", &[], &[ValType::I32]),
    ];
    let mut function_exports = 0;
    let mut memory_exports = 0;
    for export in module.exports() {
        match export.ty() {
            ExternType::Func(_) => {
                function_exports += 1;
                let Some((_, params, results)) =
                    expected.iter().find(|(name, _, _)| *name == export.name())
                else {
                    return Err(Error::abi(format!(
                        "unexpected function export {:?}",
                        export.name()
                    )));
                };
                expect_signature(export.ty(), params, results, export.name())?;
            }
            ExternType::Memory(_) if export.name() == "memory" => memory_exports += 1,
            _ => {
                return Err(Error::abi(format!("unexpected export {:?}", export.name())));
            }
        }
    }
    if function_exports != expected.len() || memory_exports != 1 {
        return Err(Error::abi(format!(
            "guest exports {function_exports} functions and {memory_exports} memories"
        )));
    }
    Ok(())
}

fn expect_signature(
    ty: ExternType,
    expected_params: &[ValType],
    expected_results: &[ValType],
    name: &str,
) -> std::result::Result<(), Error> {
    let ExternType::Func(function) = ty else {
        return Err(Error::abi(format!("{name} is not a function")));
    };
    let params: Vec<_> = function.params().collect();
    let results: Vec<_> = function.results().collect();
    if !same_types(&params, expected_params) || !same_types(&results, expected_results) {
        return Err(Error::abi(format!("{name} has an unexpected signature")));
    }
    Ok(())
}

fn same_types(actual: &[ValType], expected: &[ValType]) -> bool {
    actual.len() == expected.len()
        && actual
            .iter()
            .zip(expected)
            .all(|(actual, expected)| ValType::eq(actual, expected))
}
