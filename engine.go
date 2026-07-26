package sonnetbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/fuel"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/thevilledev/sonnetbox/internal/guestblob"
	"github.com/thevilledev/sonnetbox/internal/protocol"
)

const (
	wasmPageSize        = uint64(65536)
	minHostResponseSize = uint32(256)
	jsonnetVersion      = "v0.22.0"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var defaultConfig = EngineConfig{
	MaxMemoryBytes:           128 << 20,
	MaxFuel:                  100_000_000,
	MaxSourceBytes:           256 << 10,
	MaxOutputBytes:           1 << 20,
	MaxStack:                 256,
	MaxImports:               64,
	MaxImportBytes:           256 << 10,
	MaxTotalImportBytes:      2 << 20,
	MaxCapabilityCalls:       128,
	MaxHostRequestBytes:      512 << 10,
	MaxHostResponseBytes:     512 << 10,
	MaxTraceBytes:            64 << 10,
	MaxConcurrentEvaluations: 4,
}

var hardCeilings = EngineConfig{
	MaxMemoryBytes:           1 << 30,
	MaxFuel:                  10_000_000_000,
	MaxSourceBytes:           16 << 20,
	MaxOutputBytes:           64 << 20,
	MaxStack:                 4096,
	MaxImports:               4096,
	MaxImportBytes:           16 << 20,
	MaxTotalImportBytes:      64 << 20,
	MaxCapabilityCalls:       10000,
	MaxHostRequestBytes:      16 << 20,
	MaxHostResponseBytes:     32 << 20,
	MaxTraceBytes:            4 << 20,
	MaxConcurrentEvaluations: 1024,
}

// DefaultEngineConfig returns the ceilings that a zero-valued EngineConfig
// selects. Callers can start from these defaults, adjust individual fields,
// and pass the result to NewEngine.
func DefaultEngineConfig() EngineConfig {
	return defaultConfig
}

// Ceilings returns the library's hard maximum for every EngineConfig field.
// NewEngine rejects any configuration above these values, so an operator
// policy can never widen the sandbox beyond them.
func Ceilings() EngineConfig {
	return hardCeilings
}

// Normalize resolves zero-valued fields to their defaults and validates every
// field, returning the configuration an Engine would apply. NewEngine performs
// the same work, so Normalize lets a caller check or display an
// operator-supplied policy without paying to compile the guest.
func (c EngineConfig) Normalize() (EngineConfig, error) {
	return normalizeConfig(c)
}

// Engine owns a compiled guest module and instantiates a fresh guest for every
// evaluation. An Engine is safe for concurrent use and must be closed when it
// is no longer needed.
type Engine struct {
	mu       sync.RWMutex
	closed   bool
	config   EngineConfig
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	gate     chan struct{}
	instance atomic.Uint64
	// Set once during construction and never mutated, so evaluations read them
	// without the lock.
	defaultImporter     Importer
	defaultCapabilities map[string]Capability
	observer            *Observer
}

type invocationKey struct{}

type invocationState struct {
	mu              sync.Mutex
	request         Request
	limits          RequestLimits
	importCalls     uint32
	importBytes     uint64
	capabilityCalls uint32
	lastErr         error
}

// NewEngine compiles the embedded guest and prepares an isolated Jsonnet
// engine. Zero-valued config fields select documented defaults. The context
// controls initialization and is not retained; callers must close the returned
// Engine.
//
// Compiling the guest dominates this call. Processes that create engines
// repeatedly, such as short-lived commands, should pass
// [WithCompilationCache] to reuse compiled code.
func NewEngine(
	ctx context.Context,
	config EngineConfig,
	options ...Option,
) (*Engine, error) {
	resolved, err := newEngineOptions(options)
	if err != nil {
		return nil, err
	}
	return newEngine(ctx, config, resolved)
}

func newEngine(
	ctx context.Context,
	config EngineConfig,
	options engineOptions,
) (*Engine, error) {
	if ctx == nil {
		return nil, &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	// Compilation and the ABI probe both run under this context. Reporting the
	// cancellation here keeps callers from seeing it as a guest ABI failure.
	if err := ctx.Err(); err != nil {
		return nil, &CancellationError{Err: err}
	}
	runtimeConfig := options.runtimeConfig()
	effective, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	pages := effective.MaxMemoryBytes / wasmPageSize
	if pages == 0 || pages > math.MaxUint32 {
		return nil, &InvalidRequestError{
			Field: "MaxMemoryBytes",
			Err:   errors.New("memory limit cannot be represented as WASM pages"),
		}
	}

	runtimeConfig = runtimeConfig.
		WithMemoryLimitPages(uint32(pages)).
		WithCloseOnContextDone(true)
	r := wazero.NewRuntimeWithConfig(fuel.WithEnabled(ctx), runtimeConfig)
	cleanupCtx := context.WithoutCancel(ctx)
	cleanup := true
	defer func(cleanupCtx context.Context) {
		if cleanup {
			_ = r.Close(cleanupCtx)
		}
	}(cleanupCtx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate WASI: %w", err)}
	}
	e := &Engine{
		config:              effective,
		runtime:             r,
		gate:                make(chan struct{}, effective.MaxConcurrentEvaluations),
		defaultImporter:     options.defaultImporter,
		defaultCapabilities: options.defaultCapabilities,
		observer:            options.observer,
	}
	if err := e.instantiateHostModule(ctx); err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate host ABI: %w", err)}
	}
	compiled, err := r.CompileModule(ctx, guestblob.Module)
	if err != nil {
		return nil, &ABIError{Err: fmt.Errorf("compile embedded guest: %w", err)}
	}
	if err := validateCompiledModule(compiled); err != nil {
		_ = compiled.Close(cleanupCtx)
		return nil, &ABIError{Err: fmt.Errorf("validate embedded guest: %w", err)}
	}
	e.compiled = compiled

	probeCtx, _ := fuel.WithFuel(ctx, math.MaxUint64)
	probe, err := e.instantiate(probeCtx)
	if err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate ABI probe: %w", err)}
	}
	version, err := callU32(probeCtx, probe, "sonnetbox_abi_version")
	_ = probe.Close(cleanupCtx)
	if err != nil {
		return nil, &ABIError{Err: err}
	}
	if err := validateABIVersion(version); err != nil {
		return nil, err
	}
	cleanup = false
	return e, nil
}

func validateABIVersion(version uint32) error {
	if version == protocol.ABIVersion {
		return nil
	}
	return &ABIError{
		Err: fmt.Errorf("version %d does not match host version %d", version, protocol.ABIVersion),
	}
}

// Config returns the effective configuration after defaults were applied and
// validation passed. It is useful for logging or reporting the policy that is
// actually in force, which may differ from the requested configuration.
func (e *Engine) Config() EngineConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config
}

// Version reports the embedded go-jsonnet evaluator and private host/guest ABI
// versions.
func Version() VersionInfo {
	return VersionInfo{
		Jsonnet: jsonnetVersion,
		ABI:     protocol.ABIVersion,
	}
}

// Evaluate evaluates Request.Source in a fresh guest instance, using
// Request.Filename as the base for relative imports. It is safe to call
// concurrently.
func (e *Engine) Evaluate(ctx context.Context, request Request) (Result, error) {
	return e.evaluate(ctx, request, protocol.InputSnippet)
}

// EvaluateAnonymous evaluates Request.Source in a fresh guest instance.
// Request.Filename is used only for diagnostics; imports are resolved from the
// importer's root.
func (e *Engine) EvaluateAnonymous(
	ctx context.Context,
	request Request,
) (Result, error) {
	return e.evaluate(ctx, request, protocol.InputAnonymous)
}

// EvaluateFile loads filename through Request.Importer and evaluates it in a
// fresh guest instance. The filename must be a canonical virtual path, and
// Request.Source must be empty.
func (e *Engine) EvaluateFile(
	ctx context.Context,
	filename string,
	request Request,
) (Result, error) {
	if request.Source != "" {
		return Result{}, &InvalidRequestError{
			Field: "Source",
			Err:   errors.New("must be empty for file evaluation"),
		}
	}
	if request.Filename != "" && request.Filename != filename {
		return Result{}, &InvalidRequestError{
			Field: "Filename",
			Err:   errors.New("conflicts with EvaluateFile filename"),
		}
	}
	request.Filename = filename
	return e.evaluate(ctx, request, protocol.InputFile)
}

// withDefaults fills in the engine-level importer and capabilities configured
// by the operator. A request wins where the two overlap, so a caller can
// substitute its own importer or replace a capability by name, but cannot
// remove a default it did not supply a replacement for.
func (e *Engine) withDefaults(request Request) Request {
	if request.Importer == nil {
		request.Importer = e.defaultImporter
	}
	if len(e.defaultCapabilities) == 0 {
		return request
	}
	merged := make(
		map[string]Capability,
		len(e.defaultCapabilities)+len(request.Capabilities),
	)
	maps.Copy(merged, e.defaultCapabilities)
	maps.Copy(merged, request.Capabilities)
	request.Capabilities = merged
	return request
}

func (e *Engine) evaluate(
	ctx context.Context,
	request Request,
	inputMode protocol.InputMode,
) (result Result, err error) {
	if ctx == nil {
		return Result{}, &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	// Named results let one deferred hook observe every outcome, including the
	// validation failures that never reach the guest.
	filename := request.Filename
	defer func() {
		e.observeEvaluation(ctx, EvaluationEvent{
			Filename: filename,
			Stats:    result.Stats,
			Err:      err,
		})
	}()
	if err := ctx.Err(); err != nil {
		return Result{}, &CancellationError{Err: err}
	}
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return Result{}, &EngineClosedError{}
	}
	config := e.config
	e.mu.RUnlock()

	wireRequest, normalized, err := prepareRequestMode(e.withDefaults(request), config, inputMode)
	if err != nil {
		return Result{}, err
	}
	filename = normalized.Filename

	queueStarted := time.Now()
	if err := e.acquire(ctx); err != nil {
		return Result{}, err
	}
	queueDuration := time.Since(queueStarted)
	defer e.release()

	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return Result{}, &EngineClosedError{}
	}
	compiled := e.compiled
	r := e.runtime
	e.mu.RUnlock()

	executionStarted := time.Now()
	state := &invocationState{request: normalized, limits: normalized.Limits}
	callCtx := context.WithValue(ctx, invocationKey{}, state)
	callCtx, meter := fuel.WithFuel(callCtx, normalized.Limits.MaxFuel)

	mod, err := r.InstantiateModule(callCtx, compiled, e.moduleConfig())
	if err != nil {
		return Result{}, e.runtimeError(ctx, "initialize", normalized.Limits.MaxFuel, err)
	}
	cleanupCtx := context.WithoutCancel(callCtx)
	defer func(cleanupCtx context.Context) {
		_ = mod.Close(cleanupCtx)
	}(cleanupCtx)

	alloc := mod.ExportedFunction("sonnetbox_request_alloc")
	if alloc == nil {
		return Result{}, &ABIError{Err: errors.New("missing sonnetbox_request_alloc export")}
	}
	requestLength := uint32(len(wireRequest)) //nolint:gosec // prepareRequest bounds this length to uint32.
	values, err := alloc.Call(callCtx, uint64(requestLength))
	if err != nil {
		return Result{}, e.runtimeError(ctx, "request allocation", normalized.Limits.MaxFuel, err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 || values[0] == 0 {
		return Result{}, &ABIError{Err: errors.New("guest returned invalid request pointer")}
	}
	requestPtr := uint32(values[0]) //nolint:gosec // the preceding check rejects values outside uint32.
	if ok := mod.Memory().Write(requestPtr, wireRequest); !ok {
		return Result{}, &ABIError{Err: errors.New("request pointer is outside guest memory")}
	}

	evaluate := mod.ExportedFunction("sonnetbox_evaluate")
	if evaluate == nil {
		return Result{}, &ABIError{Err: errors.New("missing sonnetbox_evaluate export")}
	}
	values, err = evaluate.Call(callCtx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, &CancellationError{Err: ctxErr}
		}
		return Result{}, e.runtimeError(ctx, "evaluation", normalized.Limits.MaxFuel, err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 {
		return Result{}, &ABIError{Err: errors.New("guest returned invalid evaluation status")}
	}
	status := uint32(values[0]) //nolint:gosec // the preceding check rejects values outside uint32.

	// The guest fills its trace buffer before reporting an error, so read the
	// trace whatever the status. A trace is often the only evidence of why a
	// template failed, and discarding it on failure hides it exactly when it
	// matters most.
	var trace []byte
	var traceTruncated bool
	if normalized.CaptureTrace {
		trace, traceTruncated, err = e.readGuestTrace(
			callCtx,
			mod,
			normalized.Limits.MaxTraceBytes,
			normalized.Limits.MaxFuel,
		)
		if err != nil {
			return Result{}, err
		}
	}
	partial := func() Result {
		return Result{
			Trace: trace,
			Stats: state.stats(
				queueDuration,
				time.Since(executionStarted),
				normalized.Limits.MaxFuel-meter.Remaining(),
				len(trace),
				traceTruncated,
			),
		}
	}

	if hostErr := state.error(); status == protocol.EvalHostError && hostErr != nil {
		return partial(), hostErr
	}

	ptr, err := e.callU32(
		callCtx,
		mod,
		"sonnetbox_result_ptr",
		normalized.Limits.MaxFuel,
	)
	if err != nil {
		return partial(), err
	}
	length, err := e.callU32(
		callCtx,
		mod,
		"sonnetbox_result_len",
		normalized.Limits.MaxFuel,
	)
	if err != nil {
		return partial(), err
	}
	maxResult := normalized.Limits.MaxHostResponseBytes
	if status == protocol.EvalOK {
		maxResult = normalized.Limits.MaxOutputBytes
	}
	payload, err := readGuestResult(mod, ptr, length, maxResult)
	if err != nil {
		return partial(), err
	}
	if status == protocol.EvalOK {
		result, err := decodeResult(normalized.OutputMode, payload)
		if err != nil {
			return partial(), err
		}
		result.Trace = trace
		result.Stats = partial().Stats
		return result, nil
	}
	return partial(), guestStatusError(status, payload, normalized.Limits)
}

func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return &CancellationError{Err: ctx.Err()}
	}
}

func (e *Engine) release() {
	<-e.gate
}

// Close rejects new evaluations and closes the runtime. It is idempotent and
// aborts active guest calls.
func (e *Engine) Close(ctx context.Context) error {
	if ctx == nil {
		return &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	r := e.runtime
	e.mu.Unlock()
	return r.Close(ctx)
}

func (e *Engine) instantiate(ctx context.Context) (api.Module, error) {
	return e.runtime.InstantiateModule(ctx, e.compiled, e.moduleConfig())
}

func (e *Engine) moduleConfig() wazero.ModuleConfig {
	id := e.instance.Add(1)
	return wazero.NewModuleConfig().
		WithName(fmt.Sprintf("sonnetbox-%d", id)).
		WithStartFunctions("_initialize")
}

func (e *Engine) instantiateHostModule(ctx context.Context) error {
	_, err := e.runtime.NewHostModuleBuilder("sonnetbox_host").
		NewFunctionBuilder().
		WithFunc(e.hostCall).
		Export("call").
		Instantiate(ctx)
	return err
}

func (e *Engine) hostCall(
	ctx context.Context,
	mod api.Module,
	operation, requestPtr, requestLen, responsePtr, responseCap uint32,
) (packed uint64) {
	state, ok := ctx.Value(invocationKey{}).(*invocationState)
	if !ok || state == nil {
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	response, err := hostResponseBuffer(
		mod,
		responsePtr,
		responseCap,
		state.limits.MaxHostResponseBytes,
	)
	if err != nil {
		state.record(&ABIError{Err: err})
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	raw, err := readHostRequest(mod, requestPtr, requestLen, state.limits.MaxHostRequestBytes)
	if err != nil {
		state.record(&ABIError{Err: err})
		return protocol.Pack(protocol.HostMalformed, 0)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			var recoveredErr error
			switch operation {
			case protocol.OperationResolveImport:
				recoveredErr = &ImportError{Err: fmt.Errorf("handler panicked: %v", recovered)}
			case protocol.OperationCallCapability:
				recoveredErr = &CapabilityError{
					Name: "<unknown>",
					Err:  fmt.Errorf("handler panicked: %v", recovered),
				}
			default:
				recoveredErr = &ABIError{Err: fmt.Errorf("host operation %d panicked: %v", operation, recovered)}
			}
			state.record(recoveredErr)
			packed = writeHostResponse(response, protocol.HostHandlerFailure, []byte(recoveredErr.Error()))
		}
	}()

	var status uint32
	var payload []byte
	var callErr error
	switch operation {
	case protocol.OperationResolveImport:
		status, payload, callErr = e.resolveImport(ctx, state, raw)
	case protocol.OperationCallCapability:
		status, payload, callErr = e.callCapability(ctx, state, raw)
	default:
		callErr = &ABIError{Err: fmt.Errorf("unknown host operation %d", operation)}
		status = protocol.HostMalformed
		payload = []byte(callErr.Error())
	}
	if callErr != nil {
		state.record(callErr)
	}
	if status == protocol.HostOK && len(payload) > len(response) {
		limit := &LimitError{
			Resource: "host response capacity",
			Limit:    uint64(len(response)),
			Actual:   uint64(len(payload)),
		}
		state.record(limit)
		return writeHostResponse(response, protocol.HostLimit, []byte(limit.Error()))
	}
	return writeHostResponse(response, status, payload)
}

func (e *Engine) resolveImport(
	ctx context.Context,
	state *invocationState,
	raw []byte,
) (status uint32, payload []byte, err error) {
	var request protocol.ImportRequest
	// Named results let one deferred hook observe every outcome below without
	// instrumenting each return.
	var resolvedPath string
	var servedBytes int
	started := time.Now()
	defer func() {
		e.observeImport(ctx, ImportEvent{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			ResolvedPath: resolvedPath,
			Bytes:        servedBytes,
			Duration:     time.Since(started),
			Denied:       status == protocol.HostDenied,
			Err:          err,
		})
	}()
	if err := protocol.DecodeJSON(raw, &request); err != nil {
		wrapped := &ABIError{Err: fmt.Errorf("decode import request: %w", err)}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}
	if request.ImportedPath == "" {
		wrapped := &ABIError{Err: errors.New("import request path is empty")}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}
	if request.ImportedFrom != "" {
		if err := validateVirtualPath(request.ImportedFrom); err != nil {
			wrapped := &ABIError{Err: fmt.Errorf("invalid importing path: %w", err)}
			return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
		}
	}
	if err := validateImportPath(request.ImportedPath); err != nil {
		denied := &ImportDeniedError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          err,
		}
		return protocol.HostDenied, []byte(denied.Error()), denied
	}

	state.mu.Lock()
	if state.importCalls >= state.limits.MaxImports {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "import count",
			Limit:    uint64(state.limits.MaxImports),
			Actual:   uint64(state.importCalls) + 1,
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.importCalls++
	importer := state.request.Importer
	state.mu.Unlock()
	if importer == nil {
		denied := &ImportDeniedError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          ErrImportDenied,
		}
		return protocol.HostDenied, []byte(denied.Error()), denied
	}

	canonical, content, importErr := importer.Import(ctx, request.ImportedFrom, request.ImportedPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		canceled := &CancellationError{Err: ctxErr}
		return protocol.HostCanceled, []byte(canceled.Error()), canceled
	}
	if importErr != nil {
		var wrapped error
		status := protocol.HostHandlerFailure
		if errors.Is(importErr, ErrImportDenied) {
			status = protocol.HostDenied
			wrapped = &ImportDeniedError{
				ImportedFrom: request.ImportedFrom,
				ImportedPath: request.ImportedPath,
				Err:          importErr,
			}
		} else {
			wrapped = &ImportError{
				ImportedFrom: request.ImportedFrom,
				ImportedPath: request.ImportedPath,
				Err:          importErr,
			}
		}
		return status, []byte(wrapped.Error()), wrapped
	}
	if err := validateVirtualPath(canonical); err != nil {
		wrapped := &ImportError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          fmt.Errorf("invalid canonical path %q: %w", canonical, err),
		}
		return protocol.HostHandlerFailure, []byte(wrapped.Error()), wrapped
	}
	if uint64(len(content)) > uint64(state.limits.MaxImportBytes) {
		limit := &LimitError{
			Resource: "import bytes",
			Limit:    uint64(state.limits.MaxImportBytes),
			Actual:   uint64(len(content)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.mu.Lock()
	nextTotal := state.importBytes + uint64(len(content))
	if nextTotal < state.importBytes || nextTotal > state.limits.MaxTotalImportBytes {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "total import bytes",
			Limit:    state.limits.MaxTotalImportBytes,
			Actual:   nextTotal,
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.importBytes = nextTotal
	state.mu.Unlock()

	content = bytes.Clone(content)
	encoded, err := json.Marshal(protocol.ImportResponse{
		Canonical: canonical,
		Content:   content,
	})
	if err != nil {
		wrapped := &ImportError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          err,
		}
		return protocol.HostHandlerFailure, []byte(wrapped.Error()), wrapped
	}
	if uint64(len(encoded)) > uint64(state.limits.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "import response bytes",
			Limit:    uint64(state.limits.MaxHostResponseBytes),
			Actual:   uint64(len(encoded)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	resolvedPath = canonical
	servedBytes = len(content)
	return protocol.HostOK, encoded, nil
}

func (e *Engine) callCapability(
	ctx context.Context,
	state *invocationState,
	raw []byte,
) (status uint32, payload []byte, err error) {
	var request protocol.CapabilityRequest
	// Named results let one deferred hook observe every outcome below.
	started := time.Now()
	defer func() {
		e.observeCapability(ctx, CapabilityEvent{
			Name:     request.Name,
			Args:     len(request.Args),
			Duration: time.Since(started),
			Err:      err,
		})
	}()
	if err := protocol.DecodeJSON(raw, &request); err != nil {
		wrapped := &ABIError{Err: fmt.Errorf("decode capability request: %w", err)}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}
	if request.Name == "" {
		wrapped := &ABIError{Err: errors.New("capability request name is empty")}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}

	state.mu.Lock()
	if state.capabilityCalls >= state.limits.MaxCapabilityCalls {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "capability calls",
			Limit:    uint64(state.limits.MaxCapabilityCalls),
			Actual:   uint64(state.capabilityCalls) + 1,
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.capabilityCalls++
	capability, ok := state.request.Capabilities[request.Name]
	state.mu.Unlock()
	if !ok {
		failure := &CapabilityError{Name: request.Name, Err: errors.New("capability is not registered")}
		return protocol.HostDenied, []byte(failure.Error()), failure
	}

	value, callErr := capability.Call(ctx, request.Args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		canceled := &CancellationError{Err: ctxErr}
		return protocol.HostCanceled, []byte(canceled.Error()), canceled
	}
	if callErr != nil {
		failure := &CapabilityError{Name: request.Name, Err: callErr}
		return protocol.HostHandlerFailure, []byte(failure.Error()), failure
	}
	encodedValue, err := json.Marshal(value)
	if err != nil {
		failure := &CapabilityError{Name: request.Name, Err: fmt.Errorf("result is not JSON-compatible: %w", err)}
		return protocol.HostHandlerFailure, []byte(failure.Error()), failure
	}
	encoded, err := json.Marshal(protocol.CapabilityResponse{Value: encodedValue})
	if err != nil {
		failure := &CapabilityError{Name: request.Name, Err: fmt.Errorf("encode result envelope: %w", err)}
		return protocol.HostHandlerFailure, []byte(failure.Error()), failure
	}
	if uint64(len(encoded)) > uint64(state.limits.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "capability response bytes",
			Limit:    uint64(state.limits.MaxHostResponseBytes),
			Actual:   uint64(len(encoded)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	return protocol.HostOK, encoded, nil
}

var allowedWASIImports = map[string]struct{}{
	"args_get":            {},
	"args_sizes_get":      {},
	"clock_time_get":      {},
	"environ_get":         {},
	"environ_sizes_get":   {},
	"fd_close":            {},
	"fd_fdstat_get":       {},
	"fd_fdstat_set_flags": {},
	"fd_filestat_get":     {},
	"fd_prestat_dir_name": {},
	"fd_prestat_get":      {},
	"fd_read":             {},
	"fd_write":            {},
	"path_filestat_get":   {},
	"path_open":           {},
	"poll_oneoff":         {},
	"proc_exit":           {},
	"random_get":          {},
	"sched_yield":         {},
}

var requiredGuestExports = map[string]struct {
	params  []api.ValueType
	results []api.ValueType
}{
	"_initialize":             {},
	"sonnetbox_abi_version":   {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_request_alloc": {params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_evaluate":      {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_result_ptr":    {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_result_len":    {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_trace_ptr":     {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_trace_len":     {results: []api.ValueType{api.ValueTypeI32}},
	"sonnetbox_trace_truncated": {
		results: []api.ValueType{api.ValueTypeI32},
	},
}

func validateCompiledModule(compiled wazero.CompiledModule) error {
	if len(compiled.ImportedMemories()) != 0 {
		return errors.New("guest imports memory")
	}
	hostCalls := 0
	for _, definition := range compiled.ImportedFunctions() {
		moduleName, functionName, ok := definition.Import()
		if !ok {
			return fmt.Errorf("function %q is not marked as an import", definition.DebugName())
		}
		switch moduleName {
		case wasi_snapshot_preview1.ModuleName:
			if _, ok := allowedWASIImports[functionName]; !ok {
				return fmt.Errorf("guest imports unexpected WASI function %q", functionName)
			}
		case "sonnetbox_host":
			hostCalls++
			if functionName != "call" {
				return fmt.Errorf("guest imports unexpected host function %q", functionName)
			}
			wantParams := []api.ValueType{
				api.ValueTypeI32,
				api.ValueTypeI32,
				api.ValueTypeI32,
				api.ValueTypeI32,
				api.ValueTypeI32,
			}
			wantResults := []api.ValueType{api.ValueTypeI64}
			if !slices.Equal(definition.ParamTypes(), wantParams) ||
				!slices.Equal(definition.ResultTypes(), wantResults) {
				return errors.New("sonnetbox_host.call has an unexpected signature")
			}
		default:
			return fmt.Errorf(
				"guest imports unexpected function %s.%s",
				moduleName,
				functionName,
			)
		}
	}
	if hostCalls != 1 {
		return fmt.Errorf("guest imports sonnetbox_host.call %d times, want 1", hostCalls)
	}

	exports := compiled.ExportedFunctions()
	if len(exports) != len(requiredGuestExports) {
		return fmt.Errorf("guest exports %d functions, want %d", len(exports), len(requiredGuestExports))
	}
	for name, expected := range requiredGuestExports {
		definition, ok := exports[name]
		if !ok {
			return fmt.Errorf("guest is missing function export %q", name)
		}
		if !slices.Equal(definition.ParamTypes(), expected.params) ||
			!slices.Equal(definition.ResultTypes(), expected.results) {
			return fmt.Errorf("guest export %q has an unexpected signature", name)
		}
	}
	memories := compiled.ExportedMemories()
	if len(memories) != 1 || memories["memory"] == nil {
		return errors.New(`guest must export exactly one memory named "memory"`)
	}
	return nil
}

func normalizeConfig(input EngineConfig) (EngineConfig, error) {
	out := input
	var err error
	out.MaxMemoryBytes, err = normalizeUnsigned(
		"MaxMemoryBytes",
		out.MaxMemoryBytes,
		defaultConfig.MaxMemoryBytes,
		hardCeilings.MaxMemoryBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxFuel, err = normalizeUnsigned(
		"MaxFuel",
		out.MaxFuel,
		defaultConfig.MaxFuel,
		hardCeilings.MaxFuel,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxSourceBytes, err = normalizeUnsigned(
		"MaxSourceBytes",
		out.MaxSourceBytes,
		defaultConfig.MaxSourceBytes,
		hardCeilings.MaxSourceBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxOutputBytes, err = normalizeUnsigned(
		"MaxOutputBytes",
		out.MaxOutputBytes,
		defaultConfig.MaxOutputBytes,
		hardCeilings.MaxOutputBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxStack, err = normalizeStack(out.MaxStack)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxImports, err = normalizeUnsigned(
		"MaxImports",
		out.MaxImports,
		defaultConfig.MaxImports,
		hardCeilings.MaxImports,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxImportBytes, err = normalizeUnsigned(
		"MaxImportBytes",
		out.MaxImportBytes,
		defaultConfig.MaxImportBytes,
		hardCeilings.MaxImportBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxTotalImportBytes, err = normalizeUnsigned(
		"MaxTotalImportBytes",
		out.MaxTotalImportBytes,
		defaultConfig.MaxTotalImportBytes,
		hardCeilings.MaxTotalImportBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxCapabilityCalls, err = normalizeUnsigned(
		"MaxCapabilityCalls",
		out.MaxCapabilityCalls,
		defaultConfig.MaxCapabilityCalls,
		hardCeilings.MaxCapabilityCalls,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxHostRequestBytes, err = normalizeUnsigned(
		"MaxHostRequestBytes",
		out.MaxHostRequestBytes,
		defaultConfig.MaxHostRequestBytes,
		hardCeilings.MaxHostRequestBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxHostResponseBytes, err = normalizeUnsigned(
		"MaxHostResponseBytes",
		out.MaxHostResponseBytes,
		defaultConfig.MaxHostResponseBytes,
		hardCeilings.MaxHostResponseBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxTraceBytes, err = normalizeUnsigned(
		"MaxTraceBytes",
		out.MaxTraceBytes,
		defaultConfig.MaxTraceBytes,
		hardCeilings.MaxTraceBytes,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	out.MaxConcurrentEvaluations, err = normalizeUnsigned(
		"MaxConcurrentEvaluations",
		out.MaxConcurrentEvaluations,
		defaultConfig.MaxConcurrentEvaluations,
		hardCeilings.MaxConcurrentEvaluations,
	)
	if err != nil {
		return EngineConfig{}, err
	}
	if out.MaxMemoryBytes%wasmPageSize != 0 {
		return EngineConfig{}, &InvalidRequestError{
			Field: "EngineConfig.MaxMemoryBytes",
			Err:   fmt.Errorf("must be a multiple of %d bytes", wasmPageSize),
		}
	}
	if out.MaxHostResponseBytes < minHostResponseSize {
		return EngineConfig{}, &InvalidRequestError{
			Field: "EngineConfig.MaxHostResponseBytes",
			Err:   fmt.Errorf("must be at least %d bytes", minHostResponseSize),
		}
	}
	return out, nil
}

func normalizeRequestLimits(
	input RequestLimits,
	ceiling EngineConfig,
) (RequestLimits, error) {
	out := input
	var err error
	out.MaxFuel, err = inheritRequestLimit(
		"MaxFuel",
		out.MaxFuel,
		ceiling.MaxFuel,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxSourceBytes, err = inheritRequestLimit(
		"MaxSourceBytes",
		out.MaxSourceBytes,
		ceiling.MaxSourceBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxOutputBytes, err = inheritRequestLimit(
		"MaxOutputBytes",
		out.MaxOutputBytes,
		ceiling.MaxOutputBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	switch {
	case out.MaxStack == 0:
		out.MaxStack = ceiling.MaxStack
	case out.MaxStack < 0:
		return RequestLimits{}, &InvalidRequestError{
			Field: "Limits.MaxStack",
			Err:   errors.New("must not be negative"),
		}
	case out.MaxStack > ceiling.MaxStack:
		return RequestLimits{}, &InvalidRequestError{
			Field: "Limits.MaxStack",
			Err:   fmt.Errorf("%d exceeds engine ceiling %d", out.MaxStack, ceiling.MaxStack),
		}
	}
	out.MaxImports, err = inheritRequestLimit(
		"MaxImports",
		out.MaxImports,
		ceiling.MaxImports,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxImportBytes, err = inheritRequestLimit(
		"MaxImportBytes",
		out.MaxImportBytes,
		ceiling.MaxImportBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxTotalImportBytes, err = inheritRequestLimit(
		"MaxTotalImportBytes",
		out.MaxTotalImportBytes,
		ceiling.MaxTotalImportBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxCapabilityCalls, err = inheritRequestLimit(
		"MaxCapabilityCalls",
		out.MaxCapabilityCalls,
		ceiling.MaxCapabilityCalls,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxHostRequestBytes, err = inheritRequestLimit(
		"MaxHostRequestBytes",
		out.MaxHostRequestBytes,
		ceiling.MaxHostRequestBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	out.MaxHostResponseBytes, err = inheritRequestLimit(
		"MaxHostResponseBytes",
		out.MaxHostResponseBytes,
		ceiling.MaxHostResponseBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	if out.MaxHostResponseBytes < minHostResponseSize {
		return RequestLimits{}, &InvalidRequestError{
			Field: "Limits.MaxHostResponseBytes",
			Err:   fmt.Errorf("must be at least %d bytes", minHostResponseSize),
		}
	}
	out.MaxTraceBytes, err = inheritRequestLimit(
		"MaxTraceBytes",
		out.MaxTraceBytes,
		ceiling.MaxTraceBytes,
	)
	if err != nil {
		return RequestLimits{}, err
	}
	return out, nil
}

func inheritRequestLimit[T ~uint32 | ~uint64](
	field string,
	value, ceiling T,
) (T, error) {
	if value == 0 {
		return ceiling, nil
	}
	if value > ceiling {
		return 0, &InvalidRequestError{
			Field: "Limits." + field,
			Err:   fmt.Errorf("%d exceeds engine ceiling %d", value, ceiling),
		}
	}
	return value, nil
}

func normalizeUnsigned[T ~uint32 | ~uint64](
	field string,
	value, fallback, ceiling T,
) (T, error) {
	if value == 0 {
		return fallback, nil
	}
	if value > ceiling {
		return 0, &InvalidRequestError{
			Field: "EngineConfig." + field,
			Err:   fmt.Errorf("%d exceeds maximum %d", value, ceiling),
		}
	}
	return value, nil
}

func normalizeStack(value int) (int, error) {
	switch {
	case value == 0:
		return defaultConfig.MaxStack, nil
	case value < 0:
		return 0, &InvalidRequestError{
			Field: "EngineConfig.MaxStack",
			Err:   errors.New("must not be negative"),
		}
	case value > hardCeilings.MaxStack:
		return 0, &InvalidRequestError{
			Field: "EngineConfig.MaxStack",
			Err:   fmt.Errorf("%d exceeds maximum %d", value, hardCeilings.MaxStack),
		}
	default:
		return value, nil
	}
}

func prepareRequest(request Request, config EngineConfig) ([]byte, Request, error) {
	return prepareRequestMode(request, config, protocol.InputSnippet)
}

func prepareRequestMode(
	request Request,
	config EngineConfig,
	inputMode protocol.InputMode,
) ([]byte, Request, error) {
	if request.Filename == "" {
		request.Filename = "snippet.jsonnet"
	}
	if err := validateVirtualPath(request.Filename); err != nil {
		return nil, Request{}, &InvalidRequestError{Field: "Filename", Err: err}
	}
	if request.OutputMode > OutputModeStream {
		return nil, Request{}, &InvalidRequestError{
			Field: "OutputMode",
			Err:   fmt.Errorf("unknown output mode %d", request.OutputMode),
		}
	}
	if inputMode > protocol.InputAnonymous {
		return nil, Request{}, &InvalidRequestError{
			Field: "input mode",
			Err:   fmt.Errorf("unknown input mode %d", inputMode),
		}
	}
	if inputMode == protocol.InputFile && request.Source != "" {
		return nil, Request{}, &InvalidRequestError{
			Field: "Source",
			Err:   errors.New("must be empty for file evaluation"),
		}
	}
	limits, err := normalizeRequestLimits(request.Limits, config)
	if err != nil {
		return nil, Request{}, err
	}
	request.Limits = limits
	if !utf8.ValidString(request.Source) {
		return nil, Request{}, &InvalidRequestError{
			Field: "Source",
			Err:   errors.New("must be valid UTF-8"),
		}
	}
	if uint64(len(request.Source)) > uint64(limits.MaxSourceBytes) {
		return nil, Request{}, &LimitError{
			Resource: "source bytes",
			Limit:    uint64(limits.MaxSourceBytes),
			Actual:   uint64(len(request.Source)),
		}
	}
	for field, values := range map[string]map[string]string{
		"ExtVars": request.ExtVars,
		"ExtCode": request.ExtCode,
		"TLAVars": request.TLAVars,
		"TLACode": request.TLACode,
	} {
		if err := validateTextMap(field, values); err != nil {
			return nil, Request{}, err
		}
	}
	descriptors := make(map[string]protocol.CapabilityDescriptor, len(request.Capabilities))
	capabilities := make(map[string]Capability, len(request.Capabilities))
	for name, capability := range request.Capabilities {
		params, err := validateCapability(name, capability)
		if err != nil {
			return nil, Request{}, err
		}
		descriptors[name] = protocol.CapabilityDescriptor{Params: params}
		capability.Params = slices.Clone(params)
		capabilities[name] = capability
	}
	request.Capabilities = capabilities
	wire := protocol.EvaluationRequest{
		Filename:      request.Filename,
		Source:        request.Source,
		InputMode:     inputMode,
		OutputMode:    protocol.OutputMode(request.OutputMode),
		ExtVars:       request.ExtVars,
		ExtCode:       request.ExtCode,
		TLAVars:       request.TLAVars,
		TLACode:       request.TLACode,
		Capabilities:  descriptors,
		StringOutput:  request.StringOutput,
		OutputNewline: !request.OmitTrailingNewline,
		CaptureTrace:  request.CaptureTrace,
		Limits: protocol.Limits{
			MaxOutputBytes:       limits.MaxOutputBytes,
			MaxStack:             limits.MaxStack,
			MaxHostRequestBytes:  limits.MaxHostRequestBytes,
			MaxHostResponseBytes: limits.MaxHostResponseBytes,
			MaxTraceBytes:        limits.MaxTraceBytes,
		},
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, Request{}, &InvalidRequestError{Err: fmt.Errorf("encode request: %w", err)}
	}
	if uint64(len(encoded)) > uint64(limits.MaxHostRequestBytes) {
		return nil, Request{}, &LimitError{
			Resource: "encoded request",
			Limit:    uint64(limits.MaxHostRequestBytes),
			Actual:   uint64(len(encoded)),
		}
	}
	return encoded, request, nil
}

func decodeResult(mode OutputMode, payload []byte) (Result, error) {
	switch mode {
	case OutputModeSingle:
		return Result{Output: payload}, nil
	case OutputModeMulti:
		files, err := protocol.DecodeMultiOutput(payload)
		if err != nil {
			return Result{}, &ABIError{Err: fmt.Errorf("decode multi-file result: %w", err)}
		}
		return Result{Files: files}, nil
	case OutputModeStream:
		documents, err := protocol.DecodeStreamOutput(payload)
		if err != nil {
			return Result{}, &ABIError{Err: fmt.Errorf("decode stream result: %w", err)}
		}
		return Result{Documents: documents}, nil
	default:
		return Result{}, &ABIError{Err: fmt.Errorf("decode unknown output mode %d", mode)}
	}
}

// validateCapability checks one declaration and returns its parameter names.
// Per-request capabilities and engine defaults share it so both are held to
// the same standard, whichever path registers them.
func validateCapability(name string, capability Capability) ([]string, error) {
	if name == "" || !utf8.ValidString(name) {
		return nil, &InvalidRequestError{
			Field: "Capabilities",
			Err:   errors.New("capability name must be nonempty UTF-8"),
		}
	}
	if capability.Call == nil {
		return nil, &InvalidRequestError{
			Field: "Capabilities." + name,
			Err:   errors.New("Call is nil"), //nolint:staticcheck // Preserve the existing public error text.
		}
	}
	seen := make(map[string]struct{}, len(capability.Params))
	for _, param := range capability.Params {
		if !identifierPattern.MatchString(param) {
			return nil, &InvalidRequestError{
				Field: "Capabilities." + name + ".Params",
				Err:   fmt.Errorf("%q is not a Jsonnet identifier", param),
			}
		}
		if _, exists := seen[param]; exists {
			return nil, &InvalidRequestError{
				Field: "Capabilities." + name + ".Params",
				Err:   fmt.Errorf("%q is duplicated", param),
			}
		}
		seen[param] = struct{}{}
	}
	return slices.Clone(capability.Params), nil
}

func validateTextMap(field string, values map[string]string) error {
	for key, value := range values {
		if !utf8.ValidString(key) {
			return &InvalidRequestError{
				Field: field,
				Err:   errors.New("contains a key that is not valid UTF-8"),
			}
		}
		if !utf8.ValidString(value) {
			return &InvalidRequestError{
				Field: field + "." + key,
				Err:   errors.New("must be valid UTF-8"),
			}
		}
	}
	return nil
}

func callU32(ctx context.Context, mod api.Module, name string) (uint32, error) {
	function := mod.ExportedFunction(name)
	if function == nil {
		return 0, fmt.Errorf("missing %s export", name)
	}
	values, err := function.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("call %s: %w", name, err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 {
		return 0, fmt.Errorf("%s returned invalid uint32", name)
	}
	return uint32(values[0]), nil //nolint:gosec // the preceding check rejects values outside uint32.
}

func (e *Engine) callU32(
	ctx context.Context,
	mod api.Module,
	name string,
	fuelLimit uint64,
) (uint32, error) {
	function := mod.ExportedFunction(name)
	if function == nil {
		return 0, &ABIError{Err: fmt.Errorf("missing %s export", name)}
	}
	values, err := function.Call(ctx)
	if err != nil {
		return 0, e.runtimeError(ctx, name, fuelLimit, err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 {
		return 0, &ABIError{Err: fmt.Errorf("%s returned invalid uint32", name)}
	}
	return uint32(values[0]), nil //nolint:gosec // the preceding check rejects values outside uint32.
}

func readHostRequest(mod api.Module, ptr, length, limit uint32) ([]byte, error) {
	if length == 0 {
		return nil, errors.New("host request is empty")
	}
	if ptr == 0 {
		return nil, errors.New("host request pointer is zero")
	}
	if length > limit {
		return nil, fmt.Errorf("host request length %d exceeds limit %d", length, limit)
	}
	memory, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil, fmt.Errorf("host request range [%d,%d) is outside guest memory", ptr, uint64(ptr)+uint64(length))
	}
	return bytes.Clone(memory), nil
}

func hostResponseBuffer(mod api.Module, ptr, capacity, limit uint32) ([]byte, error) {
	if capacity == 0 {
		return nil, errors.New("host response capacity is zero")
	}
	if ptr == 0 {
		return nil, errors.New("host response pointer is zero")
	}
	if capacity > limit {
		return nil, fmt.Errorf("host response capacity %d exceeds limit %d", capacity, limit)
	}
	memory, ok := mod.Memory().Read(ptr, capacity)
	if !ok {
		return nil, fmt.Errorf(
			"host response range [%d,%d) is outside guest memory",
			ptr,
			uint64(ptr)+uint64(capacity),
		)
	}
	return memory, nil
}

func readGuestResult(mod api.Module, ptr, length, limit uint32) ([]byte, error) {
	if length > limit {
		return nil, &LimitError{
			Resource: "guest result",
			Limit:    uint64(limit),
			Actual:   uint64(length),
		}
	}
	if length == 0 {
		if ptr != 0 {
			return nil, &ABIError{Err: errors.New("empty guest result has a nonzero pointer")}
		}
		return nil, nil
	}
	if ptr == 0 {
		return nil, &ABIError{Err: errors.New("nonempty guest result has a zero pointer")}
	}
	memory, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil, &ABIError{Err: errors.New("result pointer is outside guest memory")}
	}
	return bytes.Clone(memory), nil
}

func (e *Engine) readGuestTrace(
	ctx context.Context,
	mod api.Module,
	limit uint32,
	fuelLimit uint64,
) ([]byte, bool, error) {
	ptr, err := e.callU32(ctx, mod, "sonnetbox_trace_ptr", fuelLimit)
	if err != nil {
		return nil, false, err
	}
	length, err := e.callU32(ctx, mod, "sonnetbox_trace_len", fuelLimit)
	if err != nil {
		return nil, false, err
	}
	truncated, err := e.callU32(ctx, mod, "sonnetbox_trace_truncated", fuelLimit)
	if err != nil {
		return nil, false, err
	}
	if truncated > 1 {
		return nil, false, &ABIError{Err: errors.New("guest returned invalid trace truncation flag")}
	}
	trace, err := readGuestResult(mod, ptr, length, limit)
	if err != nil {
		var limitErr *LimitError
		if errors.As(err, &limitErr) {
			limitErr.Resource = "trace"
		}
		return nil, false, err
	}
	return trace, truncated == 1, nil
}

func writeHostResponse(response []byte, status uint32, payload []byte) uint64 {
	if status == protocol.HostOK && len(payload) > len(response) {
		return protocol.Pack(protocol.HostLimit, 0)
	}
	if status != protocol.HostOK {
		payload = validUTF8Prefix(payload, len(response))
	}
	if len(payload) > len(response) {
		payload = payload[:len(response)]
	}
	copy(response, payload)
	payloadLength := uint32(len(payload)) //nolint:gosec // payload cannot exceed the uint32-sized guest buffer.
	return protocol.Pack(status, payloadLength)
}

func validUTF8Prefix(payload []byte, limit int) []byte {
	if !utf8.Valid(payload) {
		payload = []byte(strings.ToValidUTF8(string(payload), "\uFFFD"))
	}
	if len(payload) <= limit {
		return payload
	}
	payload = payload[:limit]
	for !utf8.Valid(payload) {
		payload = payload[:len(payload)-1]
	}
	return payload
}

func (s *invocationState) record(err error) {
	s.mu.Lock()
	if s.lastErr == nil {
		s.lastErr = err
	}
	s.mu.Unlock()
}

func (s *invocationState) error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *invocationState) stats(
	queueDuration time.Duration,
	executionDuration time.Duration,
	fuelConsumed uint64,
	traceBytes int,
	traceTruncated bool,
) EvaluationStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return EvaluationStats{
		QueueDuration:     queueDuration,
		ExecutionDuration: executionDuration,
		FuelConsumed:      fuelConsumed,
		ImportResolutions: s.importCalls,
		ImportBytes:       s.importBytes,
		CapabilityCalls:   s.capabilityCalls,
		TraceBytes:        uint32(traceBytes), //nolint:gosec // bounded by a uint32 request limit.
		TraceTruncated:    traceTruncated,
	}
}

func (e *Engine) runtimeError(
	ctx context.Context,
	operation string,
	fuelLimit uint64,
	err error,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &CancellationError{Err: ctxErr}
	}
	if errors.Is(err, fuel.ErrOutOfFuel) {
		return &LimitError{
			Resource: "fuel",
			Limit:    fuelLimit,
			Actual:   fuelLimit + 1,
			Err:      err,
		}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "out of memory") ||
		strings.Contains(message, "memory.grow") ||
		strings.Contains(message, "maximum memory") ||
		strings.Contains(message, ".runtime.mallocgc") ||
		strings.Contains(message, ".__mcache_.refill") {
		return &LimitError{
			Resource: "WASM memory",
			Limit:    e.config.MaxMemoryBytes,
			Actual:   e.config.MaxMemoryBytes + 1,
			Err:      err,
		}
	}
	e.mu.RLock()
	closed := e.closed
	e.mu.RUnlock()
	if closed {
		return &EngineClosedError{Err: err}
	}
	return &GuestTrapError{Operation: operation, Err: err}
}

func guestStatusError(status uint32, payload []byte, limits RequestLimits) error {
	var guest protocol.GuestError
	if err := protocol.DecodeJSON(payload, &guest); err != nil {
		return &ABIError{Err: fmt.Errorf("decode guest error for status %d: %w", status, err)}
	}
	cause := errors.New(guest.Message)
	switch status {
	case protocol.EvalInvalidRequest:
		return &InvalidRequestError{Err: cause}
	case protocol.EvalJsonnetError:
		return &EvaluationError{Err: cause}
	case protocol.EvalHostError:
		return &ABIError{Err: fmt.Errorf("guest reported an unclassified host error: %w", cause)}
	case protocol.EvalLimit:
		limit := guest.Limit
		actual := guest.Actual
		if limit == 0 {
			limit = uint64(limits.MaxOutputBytes)
		}
		if actual == 0 {
			actual = limit + 1
		}
		return &LimitError{
			Resource: guest.Kind,
			Limit:    limit,
			Actual:   actual,
			Err:      cause,
		}
	case protocol.EvalInternal:
		return &GuestTrapError{Operation: "evaluation", Err: cause}
	default:
		return &ABIError{Err: fmt.Errorf("unknown guest status %d: %w", status, cause)}
	}
}
