package securejsonnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/thevilledev/wasmnet/internal/guestblob"
	"github.com/thevilledev/wasmnet/internal/protocol"
)

const wasmPageSize = uint64(65536)

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
	MaxHostResponseBytes: 16 << 20,
}

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

func NewEngine(ctx context.Context, config EngineConfig) (*Engine, error) {
	if ctx == nil {
		return nil, &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	effective := normalizeConfig(config)
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
	if version != protocol.ABIVersion {
		return nil, &ABIError{
			Err: fmt.Errorf("version %d does not match host version %d", version, protocol.ABIVersion),
		}
	}
	cleanup = false
	return e, nil
}

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
		WithFunc(e.resolveImport).
		Export("resolve_import").
		NewFunctionBuilder().
		WithFunc(e.callCapability).
		Export("call_capability").
		Instantiate(ctx)
	return err
}

func (e *Engine) resolveImport(
	ctx context.Context,
	mod api.Module,
	requestPtr, requestLen, responsePtr, responseCap uint32,
) uint64 {
	state, ok := ctx.Value(invocationKey{}).(*invocationState)
	if !ok || state == nil {
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	raw, err := readHostRequest(mod, requestPtr, requestLen, state.config.MaxHostRequestBytes)
	if err != nil {
		state.record(&ABIError{Err: err})
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	var request protocol.ImportRequest
	if err := decodeJSON(raw, &request); err != nil {
		state.record(&ABIError{Err: fmt.Errorf("decode import request: %w", err)})
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostMalformed, []byte(err.Error()))
	}
	if err := validateVirtualPath(request.ImportedPath); err != nil {
		denied := &ImportDeniedError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          err,
		}
		state.record(denied)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostDenied, []byte(denied.Error()))
	}

	state.mu.Lock()
	if state.importCalls >= state.config.MaxImports {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "import count",
			Limit:    uint64(state.config.MaxImports),
			Actual:   uint64(state.importCalls) + 1,
		}
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
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
		state.record(denied)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostDenied, []byte(denied.Error()))
	}

	canonical, content, importErr := importer.Import(ctx, request.ImportedFrom, request.ImportedPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		canceled := &CancellationError{Err: ctxErr}
		state.record(canceled)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostCanceled, []byte(canceled.Error()))
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
		state.record(wrapped)
		return writeHostResponse(mod, responsePtr, responseCap, status, []byte(wrapped.Error()))
	}
	if err := validateVirtualPath(canonical); err != nil {
		wrapped := &ImportError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          fmt.Errorf("invalid canonical path %q: %w", canonical, err),
		}
		state.record(wrapped)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostHandlerFailure, []byte(wrapped.Error()))
	}
	if uint64(len(content)) > uint64(state.config.MaxImportBytes) {
		limit := &LimitError{
			Resource: "import bytes",
			Limit:    uint64(state.config.MaxImportBytes),
			Actual:   uint64(len(content)),
		}
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
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
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
	}
	state.importBytes = nextTotal
	state.mu.Unlock()

	payload, err := protocol.EncodeImportResponse(canonical, content)
	if err != nil {
		wrapped := &ImportError{
			ImportedFrom: request.ImportedFrom,
			ImportedPath: request.ImportedPath,
			Err:          err,
		}
		state.record(wrapped)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostHandlerFailure, []byte(wrapped.Error()))
	}
	if uint64(len(payload)) > uint64(state.config.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "host response",
			Limit:    uint64(state.config.MaxHostResponseBytes),
			Actual:   uint64(len(payload)),
		}
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
	}
	return writeHostResponse(mod, responsePtr, responseCap, protocol.HostOK, payload)
}

func (e *Engine) callCapability(
	ctx context.Context,
	mod api.Module,
	requestPtr, requestLen, responsePtr, responseCap uint32,
) (packed uint64) {
	state, ok := ctx.Value(invocationKey{}).(*invocationState)
	if !ok || state == nil {
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err := &CapabilityError{
				Name: "<panic>",
				Err:  fmt.Errorf("handler panicked: %v", recovered),
			}
			state.record(err)
			packed = writeHostResponse(mod, responsePtr, responseCap, protocol.HostHandlerFailure, []byte(err.Error()))
		}
	}()
	raw, err := readHostRequest(mod, requestPtr, requestLen, state.config.MaxHostRequestBytes)
	if err != nil {
		state.record(&ABIError{Err: err})
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	var request protocol.CapabilityRequest
	if err := decodeJSON(raw, &request); err != nil {
		state.record(&ABIError{Err: fmt.Errorf("decode capability request: %w", err)})
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostMalformed, []byte(err.Error()))
	}

	state.mu.Lock()
	if state.capabilityCalls >= state.config.MaxCapabilityCalls {
		state.mu.Unlock()
		limit := &LimitError{
			Resource: "capability calls",
			Limit:    uint64(state.config.MaxCapabilityCalls),
			Actual:   uint64(state.capabilityCalls) + 1,
		}
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
	}
	state.capabilityCalls++
	capability, ok := state.request.Capabilities[request.Name]
	state.mu.Unlock()
	if !ok {
		failure := &CapabilityError{Name: request.Name, Err: errors.New("capability is not registered")}
		state.record(failure)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostDenied, []byte(failure.Error()))
	}

	value, callErr := capability.Call(ctx, request.Args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		canceled := &CancellationError{Err: ctxErr}
		state.record(canceled)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostCanceled, []byte(canceled.Error()))
	}
	if callErr != nil {
		failure := &CapabilityError{Name: request.Name, Err: callErr}
		state.record(failure)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostHandlerFailure, []byte(failure.Error()))
	}
	payload, err := json.Marshal(value)
	if err != nil {
		failure := &CapabilityError{Name: request.Name, Err: fmt.Errorf("result is not JSON-compatible: %w", err)}
		state.record(failure)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostHandlerFailure, []byte(failure.Error()))
	}
	if uint64(len(payload)) > uint64(state.config.MaxHostResponseBytes) {
		limit := &LimitError{
			Resource: "host response",
			Limit:    uint64(state.config.MaxHostResponseBytes),
			Actual:   uint64(len(payload)),
		}
		state.record(limit)
		return writeHostResponse(mod, responsePtr, responseCap, protocol.HostLimit, []byte(limit.Error()))
	}
	return writeHostResponse(mod, responsePtr, responseCap, protocol.HostOK, payload)
}

func normalizeConfig(input EngineConfig) EngineConfig {
	out := input
	applyUint64 := func(value *uint64, fallback, ceiling uint64) {
		if *value == 0 {
			*value = fallback
		}
		if *value > ceiling {
			*value = ceiling
		}
	}
	applyUint32 := func(value *uint32, fallback, ceiling uint32) {
		if *value == 0 {
			*value = fallback
		}
		if *value > ceiling {
			*value = ceiling
		}
	}
	applyInt := func(value *int, fallback, ceiling int) {
		if *value <= 0 {
			*value = fallback
		}
		if *value > ceiling {
			*value = ceiling
		}
	}
	applyUint64(&out.MaxMemoryBytes, defaultConfig.MaxMemoryBytes, hardCeilings.MaxMemoryBytes)
	applyUint32(&out.MaxSourceBytes, defaultConfig.MaxSourceBytes, hardCeilings.MaxSourceBytes)
	applyUint32(&out.MaxOutputBytes, defaultConfig.MaxOutputBytes, hardCeilings.MaxOutputBytes)
	applyInt(&out.MaxStack, defaultConfig.MaxStack, hardCeilings.MaxStack)
	applyUint32(&out.MaxImports, defaultConfig.MaxImports, hardCeilings.MaxImports)
	applyUint32(&out.MaxImportBytes, defaultConfig.MaxImportBytes, hardCeilings.MaxImportBytes)
	applyUint64(&out.MaxTotalImportBytes, defaultConfig.MaxTotalImportBytes, hardCeilings.MaxTotalImportBytes)
	applyUint32(&out.MaxCapabilityCalls, defaultConfig.MaxCapabilityCalls, hardCeilings.MaxCapabilityCalls)
	applyUint32(&out.MaxHostRequestBytes, defaultConfig.MaxHostRequestBytes, hardCeilings.MaxHostRequestBytes)
	applyUint32(&out.MaxHostResponseBytes, defaultConfig.MaxHostResponseBytes, hardCeilings.MaxHostResponseBytes)
	return out
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
		if name == "" {
			return nil, Request{}, &InvalidRequestError{Field: "Capabilities", Err: errors.New("capability name is empty")}
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
		Filename:     request.Filename,
		Source:       request.Source,
		ExtVars:      request.ExtVars,
		ExtCode:      request.ExtCode,
		TLAVars:      request.TLAVars,
		TLACode:      request.TLACode,
		Capabilities: descriptors,
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
	if length > limit {
		return nil, fmt.Errorf("host request length %d exceeds limit %d", length, limit)
	}
	memory, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil, fmt.Errorf("host request range [%d,%d) is outside guest memory", ptr, uint64(ptr)+uint64(length))
	}
	return append([]byte(nil), memory...), nil
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
		return nil, nil
	}
	memory, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil, &ABIError{Err: errors.New("result pointer is outside guest memory")}
	}
	return append([]byte(nil), memory...), nil
}

func writeHostResponse(mod api.Module, ptr, capacity, status uint32, payload []byte) uint64 {
	if uint64(len(payload)) > uint64(capacity) {
		return protocol.Pack(protocol.HostLimit, 0)
	}
	if len(payload) > 0 && !mod.Memory().Write(ptr, payload) {
		return protocol.Pack(protocol.HostMalformed, 0)
	}
	return protocol.Pack(status, uint32(len(payload)))
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (s *invocationState) record(err error) {
	s.mu.Lock()
	s.lastErr = err
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
	if err := decodeJSON(payload, &guest); err != nil {
		return &ABIError{Err: fmt.Errorf("decode guest error for status %d: %w", status, err)}
	}
	cause := errors.New(guest.Message)
	switch status {
	case protocol.EvalInvalidRequest:
		return &InvalidRequestError{Err: cause}
	case protocol.EvalJsonnetError:
		return &EvaluationError{Err: cause}
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
