package securejsonnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/thevilledev/wasmnet/internal/guestblob"
	"github.com/thevilledev/wasmnet/internal/protocol"
)

const (
	wasmPageSize        = uint64(65536)
	minHostResponseSize = uint32(256)
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var defaultConfig = EngineConfig{
	MaxMemoryBytes:       128 << 20,
	MaxSourceBytes:       256 << 10,
	MaxOutputBytes:       1 << 20,
	MaxStack:             256,
	MaxImports:           64,
	MaxImportBytes:       256 << 10,
	MaxTotalImportBytes:  2 << 20,
	MaxCapabilityCalls:   128,
	MaxHostRequestBytes:  512 << 10,
	MaxHostResponseBytes: 512 << 10,
}

var hardCeilings = EngineConfig{
	MaxMemoryBytes:       1 << 30,
	MaxSourceBytes:       16 << 20,
	MaxOutputBytes:       64 << 20,
	MaxStack:             4096,
	MaxImports:           4096,
	MaxImportBytes:       16 << 20,
	MaxTotalImportBytes:  64 << 20,
	MaxCapabilityCalls:   10000,
	MaxHostRequestBytes:  16 << 20,
	MaxHostResponseBytes: 32 << 20,
}

// Engine owns a compiled guest module and instantiates a fresh guest for every
// evaluation.
type Engine struct {
	mu       sync.RWMutex
	closed   bool
	config   EngineConfig
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

type invocationKey struct{}

type invocationState struct {
	mu              sync.Mutex
	request         Request
	config          EngineConfig
	importCalls     uint32
	importBytes     uint64
	capabilityCalls uint32
	lastErr         error
}

// NewEngine compiles the embedded guest once and prepares a concurrent-safe
// isolated Jsonnet engine.
func NewEngine(ctx context.Context, config EngineConfig) (*Engine, error) {
	if ctx == nil {
		return nil, &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
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

	runtimeConfig := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(uint32(pages)).
		WithCloseOnContextDone(true)
	r := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	cleanup := true
	defer func() {
		if cleanup {
			_ = r.Close(context.Background())
		}
	}()

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate WASI: %w", err)}
	}
	e := &Engine{config: effective, runtime: r}
	if err := e.instantiateHostModule(ctx); err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate host ABI: %w", err)}
	}
	compiled, err := r.CompileModule(ctx, guestblob.Module)
	if err != nil {
		return nil, &ABIError{Err: fmt.Errorf("compile embedded guest: %w", err)}
	}
	if err := validateCompiledModule(compiled); err != nil {
		_ = compiled.Close(context.Background())
		return nil, &ABIError{Err: fmt.Errorf("validate embedded guest: %w", err)}
	}
	e.compiled = compiled

	probe, err := e.instantiate(ctx)
	if err != nil {
		return nil, &ABIError{Err: fmt.Errorf("instantiate ABI probe: %w", err)}
	}
	version, err := callU32(ctx, probe, "securejsonnet_abi_version")
	_ = probe.Close(context.Background())
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

// Evaluate runs request in a fresh guest instance. It is safe to call
// concurrently.
func (e *Engine) Evaluate(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, &CancellationError{Err: err}
	}
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return Result{}, &EngineClosedError{}
	}
	config := e.config
	compiled := e.compiled
	r := e.runtime
	e.mu.RUnlock()

	wireRequest, normalized, err := prepareRequest(request, config)
	if err != nil {
		return Result{}, err
	}
	state := &invocationState{request: normalized, config: config}
	callCtx := context.WithValue(ctx, invocationKey{}, state)

	mod, err := r.InstantiateModule(callCtx, compiled, wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize"))
	if err != nil {
		return Result{}, e.runtimeError(ctx, "initialize", err)
	}
	defer mod.Close(context.Background())

	alloc := mod.ExportedFunction("securejsonnet_request_alloc")
	if alloc == nil {
		return Result{}, &ABIError{Err: errors.New("missing securejsonnet_request_alloc export")}
	}
	values, err := alloc.Call(callCtx, uint64(uint32(len(wireRequest))))
	if err != nil {
		return Result{}, e.runtimeError(ctx, "request allocation", err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 || values[0] == 0 {
		return Result{}, &ABIError{Err: errors.New("guest returned invalid request pointer")}
	}
	requestPtr := uint32(values[0])
	if ok := mod.Memory().Write(requestPtr, wireRequest); !ok {
		return Result{}, &ABIError{Err: errors.New("request pointer is outside guest memory")}
	}

	evaluate := mod.ExportedFunction("securejsonnet_evaluate")
	if evaluate == nil {
		return Result{}, &ABIError{Err: errors.New("missing securejsonnet_evaluate export")}
	}
	values, err = evaluate.Call(callCtx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, &CancellationError{Err: ctxErr}
		}
		return Result{}, e.runtimeError(ctx, "evaluation", err)
	}
	if len(values) != 1 || values[0] > math.MaxUint32 {
		return Result{}, &ABIError{Err: errors.New("guest returned invalid evaluation status")}
	}
	status := uint32(values[0])
	if hostErr := state.error(); status == protocol.EvalHostError && hostErr != nil {
		return Result{}, hostErr
	}

	ptr, err := callU32(callCtx, mod, "securejsonnet_result_ptr")
	if err != nil {
		return Result{}, &ABIError{Err: err}
	}
	length, err := callU32(callCtx, mod, "securejsonnet_result_len")
	if err != nil {
		return Result{}, &ABIError{Err: err}
	}
	maxResult := config.MaxHostResponseBytes
	if status == protocol.EvalOK {
		maxResult = config.MaxOutputBytes
	}
	payload, err := readGuestResult(mod, ptr, length, maxResult)
	if err != nil {
		return Result{}, err
	}
	if status == protocol.EvalOK {
		return Result{Output: payload}, nil
	}
	return Result{}, guestStatusError(status, payload, config)
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
	return e.runtime.InstantiateModule(ctx, e.compiled, wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize"))
}

func (e *Engine) instantiateHostModule(ctx context.Context) error {
	_, err := e.runtime.NewHostModuleBuilder("securejsonnet_host").
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
		state.config.MaxHostResponseBytes,
	)
	if err != nil {
		state.record(&ABIError{Err: err})
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	raw, err := readHostRequest(mod, requestPtr, requestLen, state.config.MaxHostRequestBytes)
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
	if len(payload) > len(response) {
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
) (uint32, []byte, error) {
	var request protocol.ImportRequest
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
	if err := validateVirtualPath(request.ImportedPath); err != nil {
		denied := &ImportDeniedError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          err,
		}
		return protocol.HostDenied, []byte(denied.Error()), denied
	}

	state.mu.Lock()
	if state.importCalls >= state.config.MaxImports {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "import count",
			Limit:    uint64(state.config.MaxImports),
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
			Err:          errImportDenied,
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
		if errors.Is(importErr, errImportDenied) {
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
	if uint64(len(content)) > uint64(state.config.MaxImportBytes) {
		limit := &LimitError{
			Resource: "import bytes",
			Limit:    uint64(state.config.MaxImportBytes),
			Actual:   uint64(len(content)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.mu.Lock()
	nextTotal := state.importBytes + uint64(len(content))
	if nextTotal < state.importBytes || nextTotal > state.config.MaxTotalImportBytes {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "total import bytes",
			Limit:    state.config.MaxTotalImportBytes,
			Actual:   nextTotal,
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	state.importBytes = nextTotal
	state.mu.Unlock()

	content = append([]byte{}, content...)
	payload, err := json.Marshal(protocol.ImportResponse{
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
	if uint64(len(payload)) > uint64(state.config.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "import response bytes",
			Limit:    uint64(state.config.MaxHostResponseBytes),
			Actual:   uint64(len(payload)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	return protocol.HostOK, payload, nil
}

func (e *Engine) callCapability(
	ctx context.Context,
	state *invocationState,
	raw []byte,
) (uint32, []byte, error) {
	var request protocol.CapabilityRequest
	if err := protocol.DecodeJSON(raw, &request); err != nil {
		wrapped := &ABIError{Err: fmt.Errorf("decode capability request: %w", err)}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}
	if request.Name == "" {
		wrapped := &ABIError{Err: errors.New("capability request name is empty")}
		return protocol.HostMalformed, []byte(wrapped.Error()), wrapped
	}

	state.mu.Lock()
	if state.capabilityCalls >= state.config.MaxCapabilityCalls {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "capability calls",
			Limit:    uint64(state.config.MaxCapabilityCalls),
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
	payload, err := json.Marshal(protocol.CapabilityResponse{Value: encodedValue})
	if err != nil {
		failure := &CapabilityError{Name: request.Name, Err: fmt.Errorf("encode result envelope: %w", err)}
		return protocol.HostHandlerFailure, []byte(failure.Error()), failure
	}
	if uint64(len(payload)) > uint64(state.config.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "capability response bytes",
			Limit:    uint64(state.config.MaxHostResponseBytes),
			Actual:   uint64(len(payload)),
		}
		return protocol.HostLimit, []byte(limit.Error()), limit
	}
	return protocol.HostOK, payload, nil
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
	"_initialize":                 {},
	"securejsonnet_abi_version":   {results: []api.ValueType{api.ValueTypeI32}},
	"securejsonnet_request_alloc": {params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}},
	"securejsonnet_evaluate":      {results: []api.ValueType{api.ValueTypeI32}},
	"securejsonnet_result_ptr":    {results: []api.ValueType{api.ValueTypeI32}},
	"securejsonnet_result_len":    {results: []api.ValueType{api.ValueTypeI32}},
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
		case "securejsonnet_host":
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
				return errors.New("securejsonnet_host.call has an unexpected signature")
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
		return fmt.Errorf("guest imports securejsonnet_host.call %d times, want 1", hostCalls)
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
	if request.Filename == "" {
		request.Filename = "snippet.jsonnet"
	}
	if err := validateVirtualPath(request.Filename); err != nil {
		return nil, Request{}, &InvalidRequestError{Field: "Filename", Err: err}
	}
	if uint64(len(request.Source)) > uint64(config.MaxSourceBytes) {
		return nil, Request{}, &LimitError{
			Resource: "source bytes",
			Limit:    uint64(config.MaxSourceBytes),
			Actual:   uint64(len(request.Source)),
		}
	}
	descriptors := make(map[string]protocol.CapabilityDescriptor, len(request.Capabilities))
	capabilities := make(map[string]Capability, len(request.Capabilities))
	for name, capability := range request.Capabilities {
		if name == "" || !utf8.ValidString(name) {
			return nil, Request{}, &InvalidRequestError{
				Field: "Capabilities",
				Err:   errors.New("capability name must be nonempty UTF-8"),
			}
		}
		if capability.Call == nil {
			return nil, Request{}, &InvalidRequestError{Field: "Capabilities." + name, Err: errors.New("Call is nil")}
		}
		seen := make(map[string]struct{}, len(capability.Params))
		for _, param := range capability.Params {
			if !identifierPattern.MatchString(param) {
				return nil, Request{}, &InvalidRequestError{
					Field: "Capabilities." + name + ".Params",
					Err:   fmt.Errorf("%q is not a Jsonnet identifier", param),
				}
			}
			if _, exists := seen[param]; exists {
				return nil, Request{}, &InvalidRequestError{
					Field: "Capabilities." + name + ".Params",
					Err:   fmt.Errorf("%q is duplicated", param),
				}
			}
			seen[param] = struct{}{}
		}
		params := append([]string(nil), capability.Params...)
		descriptors[name] = protocol.CapabilityDescriptor{Params: params}
		capability.Params = append([]string(nil), params...)
		capabilities[name] = capability
	}
	request.Capabilities = capabilities
	wire := protocol.EvaluationRequest{
		Filename:      request.Filename,
		Source:        request.Source,
		ExtVars:       request.ExtVars,
		ExtCode:       request.ExtCode,
		TLAVars:       request.TLAVars,
		TLACode:       request.TLACode,
		Capabilities:  descriptors,
		StringOutput:  request.StringOutput,
		OutputNewline: !request.OmitTrailingNewline,
		Limits: protocol.Limits{
			MaxOutputBytes:       config.MaxOutputBytes,
			MaxStack:             config.MaxStack,
			MaxHostRequestBytes:  config.MaxHostRequestBytes,
			MaxHostResponseBytes: config.MaxHostResponseBytes,
		},
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, Request{}, &InvalidRequestError{Err: fmt.Errorf("encode request: %w", err)}
	}
	if uint64(len(encoded)) > uint64(config.MaxHostRequestBytes) {
		return nil, Request{}, &LimitError{
			Resource: "encoded request",
			Limit:    uint64(config.MaxHostRequestBytes),
			Actual:   uint64(len(encoded)),
		}
	}
	return encoded, request, nil
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
	return uint32(values[0]), nil
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
	return append([]byte(nil), memory...), nil
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
	return append([]byte(nil), memory...), nil
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
	return protocol.Pack(status, uint32(len(payload)))
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

func (e *Engine) runtimeError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &CancellationError{Err: ctxErr}
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

func guestStatusError(status uint32, payload []byte, config EngineConfig) error {
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
		return &LimitError{
			Resource: guest.Kind,
			Limit:    uint64(config.MaxOutputBytes),
			Actual:   uint64(config.MaxOutputBytes) + 1,
			Err:      cause,
		}
	case protocol.EvalInternal:
		return &GuestTrapError{Operation: "evaluation", Err: cause}
	default:
		return &ABIError{Err: fmt.Errorf("unknown guest status %d: %w", status, cause)}
	}
}
