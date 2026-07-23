//go:build wasip1 && wasm

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"unsafe"

	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/thevilledev/wasmnet/internal/protocol"
)

const maxRequestAllocation = 16 << 20
const maxErrorBytes = 64 << 10

var (
	request []byte
	result  []byte
)

//go:wasmimport securejsonnet_host resolve_import
func resolveImport(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint64

//go:wasmimport securejsonnet_host call_capability
func callCapability(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint64

//go:wasmexport securejsonnet_abi_version
func abiVersion() uint32 {
	return protocol.ABIVersion
}

//go:wasmexport securejsonnet_request_alloc
func requestAlloc(size uint32) uint32 {
	request = nil
	result = nil
	if size == 0 || size > maxRequestAllocation {
		return 0
	}
	request = make([]byte, int(size))
	return uint32(uintptr(unsafe.Pointer(&request[0])))
}

//go:wasmexport securejsonnet_evaluate
func evaluate() (status uint32) {
	defer func() {
		if recovered := recover(); recovered != nil {
			setError("internal", fmt.Sprintf("guest panic: %v", recovered))
			status = protocol.EvalInternal
		}
	}()
	if len(request) == 0 {
		setError("invalid_request", "request buffer is empty")
		return protocol.EvalInvalidRequest
	}

	var req protocol.EvaluationRequest
	if err := decodeJSON(request, &req); err != nil {
		setError("invalid_request", err.Error())
		return protocol.EvalInvalidRequest
	}
	if req.Limits.MaxStack <= 0 ||
		req.Limits.MaxHostRequestBytes == 0 ||
		req.Limits.MaxHostResponseBytes == 0 {
		setError("invalid_request", "guest limits must be positive")
		return protocol.EvalInvalidRequest
	}

	bridge := &hostBridge{
		maxRequest:  req.Limits.MaxHostRequestBytes,
		maxResponse: req.Limits.MaxHostResponseBytes,
		imports:     make(map[string]importResult),
	}
	vm := jsonnet.MakeVM()
	vm.MaxStack = req.Limits.MaxStack
	vm.Importer(bridge)
	vm.SetTraceOut(io.Discard)
	for key, value := range req.ExtVars {
		vm.ExtVar(key, value)
	}
	for key, value := range req.ExtCode {
		vm.ExtCode(key, value)
	}
	for key, value := range req.TLAVars {
		vm.TLAVar(key, value)
	}
	for key, value := range req.TLACode {
		vm.TLACode(key, value)
	}
	for name, descriptor := range req.Capabilities {
		params := make(ast.Identifiers, len(descriptor.Params))
		for i, param := range descriptor.Params {
			params[i] = ast.Identifier(param)
		}
		capabilityName := name
		vm.NativeFunction(&jsonnet.NativeFunction{
			Name:   capabilityName,
			Params: params,
			Func: func(args []any) (any, error) {
				return bridge.callCapability(capabilityName, args)
			},
		})
	}

	output, err := vm.EvaluateAnonymousSnippet(req.Filename, req.Source)
	if err != nil {
		if bridge.lastStatus != protocol.HostOK {
			setError("host", err.Error())
			return protocol.EvalHostError
		}
		setError("jsonnet", err.Error())
		return protocol.EvalJsonnetError
	}
	if uint64(len(output)) > uint64(req.Limits.MaxOutputBytes) {
		setError("output", fmt.Sprintf("output is %d bytes; limit is %d", len(output), req.Limits.MaxOutputBytes))
		return protocol.EvalLimit
	}
	result = []byte(output)
	return protocol.EvalOK
}

//go:wasmexport securejsonnet_result_ptr
func resultPtr() uint32 {
	if len(result) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&result[0])))
}

//go:wasmexport securejsonnet_result_len
func resultLen() uint32 {
	return uint32(len(result))
}

type importResult struct {
	content jsonnet.Contents
	foundAt string
	err     error
}

type hostBridge struct {
	maxRequest  uint32
	maxResponse uint32
	imports     map[string]importResult
	lastStatus  uint32
}

func (b *hostBridge) Import(importedFrom, importedPath string) (jsonnet.Contents, string, error) {
	key := importedFrom + "\x00" + importedPath
	if cached, ok := b.imports[key]; ok {
		return cached.content, cached.foundAt, cached.err
	}
	payload, err := json.Marshal(protocol.ImportRequest{
		ImportedFrom: importedFrom,
		ImportedPath: importedPath,
	})
	if err != nil {
		return jsonnet.Contents{}, "", err
	}
	response, status, err := b.callHost(payload, resolveImport)
	if err != nil {
		b.lastStatus = status
		cached := importResult{err: err}
		b.imports[key] = cached
		return cached.content, cached.foundAt, cached.err
	}
	canonical, content, err := protocol.DecodeImportResponse(response)
	if err != nil {
		b.lastStatus = protocol.HostMalformed
		err = fmt.Errorf("malformed import response: %w", err)
	}
	cached := importResult{
		content: jsonnet.MakeContentsRaw(append([]byte(nil), content...)),
		foundAt: canonical,
		err:     err,
	}
	b.imports[key] = cached
	return cached.content, cached.foundAt, cached.err
}

func (b *hostBridge) callCapability(name string, args []any) (any, error) {
	payload, err := json.Marshal(protocol.CapabilityRequest{Name: name, Args: args})
	if err != nil {
		return nil, err
	}
	response, status, err := b.callHost(payload, callCapability)
	if err != nil {
		b.lastStatus = status
		return nil, err
	}
	var value any
	if err := decodeJSON(response, &value); err != nil {
		b.lastStatus = protocol.HostMalformed
		return nil, fmt.Errorf("malformed capability response: %w", err)
	}
	return value, nil
}

type hostFunction func(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint64

func (b *hostBridge) callHost(payload []byte, call hostFunction) ([]byte, uint32, error) {
	if len(payload) == 0 || uint64(len(payload)) > uint64(b.maxRequest) {
		return nil, protocol.HostLimit, errors.New("host request limit exceeded")
	}
	response := make([]byte, int(b.maxResponse))
	packed := call(
		unsafe.Pointer(&payload[0]),
		uint32(len(payload)),
		unsafe.Pointer(&response[0]),
		uint32(len(response)),
	)
	runtime.KeepAlive(payload)
	runtime.KeepAlive(response)
	status, length := protocol.Unpack(packed)
	if length > uint32(len(response)) {
		return nil, protocol.HostMalformed, errors.New("host response length exceeds capacity")
	}
	body := append([]byte(nil), response[:length]...)
	if status != protocol.HostOK {
		message := string(body)
		if message == "" {
			message = fmt.Sprintf("host call failed with status %d", status)
		}
		return nil, status, errors.New(message)
	}
	return body, status, nil
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

func setError(kind, message string) {
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	encoded, err := json.Marshal(protocol.GuestError{Kind: kind, Message: message})
	if err != nil || len(encoded) > math.MaxUint32 {
		result = []byte(`{"kind":"internal","message":"could not encode guest error"}`)
		return
	}
	result = encoded
}

func main() {}
