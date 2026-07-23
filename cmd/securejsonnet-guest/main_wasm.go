//go:build wasip1 && wasm

// Package main builds the embedded securejsonnet WASI guest.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"unicode/utf8"
	"unsafe"

	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/thevilledev/wasmnet/internal/protocol"
)

const (
	maxRequestAllocation      = 16 << 20
	maxHostResponseAllocation = 32 << 20
	maxOutputBytes            = 64 << 20
	maxStackDepth             = 4096
	maxErrorBytes             = 64 << 10
)

var (
	request     []byte
	result      []byte
	resultLimit = maxErrorBytes
)

//go:wasmimport securejsonnet_host call
func hostCall(
	operation uint32,
	requestPtr unsafe.Pointer,
	requestLen uint32,
	responsePtr unsafe.Pointer,
	responseCap uint32,
) uint64

//go:wasmexport securejsonnet_abi_version
func abiVersion() uint32 {
	return protocol.ABIVersion
}

//go:wasmexport securejsonnet_request_alloc
func requestAlloc(size uint32) uint32 {
	request = nil
	result = nil
	resultLimit = maxErrorBytes
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
	if err := protocol.DecodeJSON(request, &req); err != nil {
		setError("invalid_request", err.Error())
		return protocol.EvalInvalidRequest
	}
	if req.Limits.MaxStack <= 0 ||
		req.Limits.MaxStack > maxStackDepth ||
		req.Limits.MaxOutputBytes == 0 ||
		req.Limits.MaxOutputBytes > maxOutputBytes ||
		req.Limits.MaxHostRequestBytes == 0 ||
		req.Limits.MaxHostResponseBytes == 0 ||
		req.Limits.MaxHostRequestBytes > maxRequestAllocation ||
		req.Limits.MaxHostResponseBytes > maxHostResponseAllocation {
		setError("invalid_request", "guest limits must be positive and within ABI ceilings")
		return protocol.EvalInvalidRequest
	}
	if req.InputMode > protocol.InputFile {
		setError("invalid_request", "unknown input mode")
		return protocol.EvalInvalidRequest
	}
	if req.OutputMode > protocol.OutputStream {
		setError("invalid_request", "unknown output mode")
		return protocol.EvalInvalidRequest
	}
	if req.InputMode == protocol.InputFile && req.Source != "" {
		setError("invalid_request", "file evaluation cannot include inline source")
		return protocol.EvalInvalidRequest
	}
	if uint64(len(request)) > uint64(req.Limits.MaxHostRequestBytes) {
		setError("invalid_request", "encoded request exceeds the guest request limit")
		return protocol.EvalInvalidRequest
	}
	resultLimit = min(maxErrorBytes, int(req.Limits.MaxHostResponseBytes))

	bridge := &hostBridge{
		maxRequest:  req.Limits.MaxHostRequestBytes,
		maxResponse: req.Limits.MaxHostResponseBytes,
		imports:     make(map[string]importResult),
		contents:    make(map[string]jsonnet.Contents),
	}
	vm := jsonnet.MakeVM()
	vm.MaxStack = req.Limits.MaxStack
	vm.StringOutput = req.StringOutput
	vm.OutputNewline = req.OutputNewline
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
		if name == "" || !utf8.ValidString(name) {
			setError("invalid_request", "capability name must be nonempty UTF-8")
			return protocol.EvalInvalidRequest
		}
		params := make(ast.Identifiers, len(descriptor.Params))
		seen := make(map[string]struct{}, len(descriptor.Params))
		for i, param := range descriptor.Params {
			if !isIdentifier(param) {
				setError("invalid_request", fmt.Sprintf("invalid capability parameter %q", param))
				return protocol.EvalInvalidRequest
			}
			if _, ok := seen[param]; ok {
				setError("invalid_request", fmt.Sprintf("duplicate capability parameter %q", param))
				return protocol.EvalInvalidRequest
			}
			seen[param] = struct{}{}
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

	output, err := evaluateRequest(vm, req)
	if err != nil {
		if bridge.localLimit != nil {
			setGuestError(*bridge.localLimit)
			return protocol.EvalLimit
		}
		if bridge.lastStatus != protocol.HostOK {
			setError("host", err.Error())
			return protocol.EvalHostError
		}
		setError("jsonnet", err.Error())
		return protocol.EvalJsonnetError
	}
	if uint64(len(output)) > uint64(req.Limits.MaxOutputBytes) {
		setGuestError(protocol.GuestError{
			Kind:    "output",
			Message: fmt.Sprintf("output is %d bytes; limit is %d", len(output), req.Limits.MaxOutputBytes),
			Limit:   uint64(req.Limits.MaxOutputBytes),
			Actual:  uint64(len(output)),
		})
		return protocol.EvalLimit
	}
	result = []byte(output)
	return protocol.EvalOK
}

func evaluateRequest(vm *jsonnet.VM, req protocol.EvaluationRequest) ([]byte, error) {
	var node ast.Node
	var err error
	if req.InputMode == protocol.InputSnippet {
		node, err = jsonnet.SnippetToAST(req.Filename, req.Source)
		if err != nil {
			return nil, errors.New(vm.ErrorFormatter.Format(err))
		}
	}

	switch req.OutputMode {
	case protocol.OutputSingle:
		var output string
		if req.InputMode == protocol.InputFile {
			output, err = vm.EvaluateFile(req.Filename)
		} else {
			output, err = vm.Evaluate(node)
			err = formatEvaluationError(vm, err)
		}
		return []byte(output), err
	case protocol.OutputMulti:
		var files map[string]string
		if req.InputMode == protocol.InputFile {
			files, err = vm.EvaluateFileMulti(req.Filename)
		} else {
			files, err = vm.EvaluateMulti(node)
			err = formatEvaluationError(vm, err)
		}
		if err != nil {
			return nil, err
		}
		return protocol.EncodeMultiOutput(files)
	case protocol.OutputStream:
		var documents []string
		if req.InputMode == protocol.InputFile {
			documents, err = vm.EvaluateFileStream(req.Filename)
		} else {
			documents, err = vm.EvaluateStream(node)
			err = formatEvaluationError(vm, err)
		}
		if err != nil {
			return nil, err
		}
		return protocol.EncodeStreamOutput(documents)
	default:
		return nil, errors.New("unknown output mode")
	}
}

func formatEvaluationError(vm *jsonnet.VM, err error) error {
	if err == nil {
		return nil
	}
	return errors.New(vm.ErrorFormatter.Format(err))
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
	response    []byte
	imports     map[string]importResult
	contents    map[string]jsonnet.Contents
	lastStatus  uint32
	localLimit  *protocol.GuestError
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
	response, status, err := b.callHost(protocol.OperationResolveImport, payload)
	if err != nil {
		b.lastStatus = status
		cached := importResult{err: err}
		b.imports[key] = cached
		return cached.content, cached.foundAt, cached.err
	}
	var decoded protocol.ImportResponse
	if decodeErr := protocol.DecodeJSON(response, &decoded); decodeErr != nil {
		b.lastStatus = protocol.HostMalformed
		err = fmt.Errorf("malformed import response: %w", decodeErr)
	} else if decoded.Canonical == "" {
		b.lastStatus = protocol.HostMalformed
		err = errors.New("malformed import response: canonical path is empty")
	}
	content, exists := b.contents[decoded.Canonical]
	if !exists {
		content = jsonnet.MakeContentsRaw(append([]byte(nil), decoded.Content...))
		b.contents[decoded.Canonical] = content
	}
	cached := importResult{content: content, foundAt: decoded.Canonical, err: err}
	b.imports[key] = cached
	return cached.content, cached.foundAt, cached.err
}

func (b *hostBridge) callCapability(name string, args []any) (any, error) {
	payload, err := json.Marshal(protocol.CapabilityRequest{Name: name, Args: args})
	if err != nil {
		return nil, err
	}
	response, status, err := b.callHost(protocol.OperationCallCapability, payload)
	if err != nil {
		b.lastStatus = status
		return nil, err
	}
	var decoded protocol.CapabilityResponse
	if err := protocol.DecodeJSON(response, &decoded); err != nil {
		b.lastStatus = protocol.HostMalformed
		return nil, fmt.Errorf("malformed capability response: %w", err)
	}
	if len(decoded.Value) == 0 {
		b.lastStatus = protocol.HostMalformed
		return nil, errors.New("malformed capability response: value is missing")
	}
	var value any
	if err := json.Unmarshal(decoded.Value, &value); err != nil {
		b.lastStatus = protocol.HostMalformed
		return nil, fmt.Errorf("malformed capability value: %w", err)
	}
	return value, nil
}

func (b *hostBridge) callHost(operation uint32, payload []byte) ([]byte, uint32, error) {
	if len(payload) == 0 || uint64(len(payload)) > uint64(b.maxRequest) {
		b.lastStatus = protocol.HostLimit
		b.localLimit = &protocol.GuestError{
			Kind:    "host request bytes",
			Message: fmt.Sprintf("host request is %d bytes; limit is %d", len(payload), b.maxRequest),
			Limit:   uint64(b.maxRequest),
			Actual:  uint64(len(payload)),
		}
		return nil, protocol.HostLimit, errors.New("host request limit exceeded")
	}
	if b.response == nil {
		b.response = make([]byte, int(b.maxResponse))
	}
	packed := hostCall(
		operation,
		unsafe.Pointer(&payload[0]),
		uint32(len(payload)),
		unsafe.Pointer(&b.response[0]),
		uint32(len(b.response)),
	)
	runtime.KeepAlive(payload)
	runtime.KeepAlive(b.response)
	status, length := protocol.Unpack(packed)
	if length > uint32(len(b.response)) {
		return nil, protocol.HostMalformed, errors.New("host response length exceeds capacity")
	}
	body := append([]byte(nil), b.response[:length]...)
	if status != protocol.HostOK {
		message := string(body)
		if message == "" {
			message = fmt.Sprintf("host call failed with status %d", status)
		}
		return nil, status, errors.New(message)
	}
	return body, status, nil
}

func setError(kind, message string) {
	setGuestError(protocol.GuestError{Kind: kind, Message: message})
}

func setGuestError(guestError protocol.GuestError) {
	guestError.Kind = truncateUTF8(guestError.Kind, 64)
	guestError.Message = truncateUTF8(guestError.Message, maxErrorBytes)
	message := guestError.Message
	encode := func(message string) ([]byte, error) {
		guestError.Message = message
		return json.Marshal(guestError)
	}
	encoded, err := encode(message)
	if err == nil && len(encoded) <= resultLimit && len(encoded) <= math.MaxUint32 {
		result = encoded
		return
	}

	low, high := 0, len(message)
	var bounded []byte
	for low <= high {
		mid := low + (high-low)/2
		candidate, candidateErr := encode(truncateUTF8(message, mid))
		if candidateErr != nil {
			err = candidateErr
			break
		}
		if len(candidate) <= resultLimit {
			bounded = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if err != nil || bounded == nil {
		result = []byte(`{"kind":"internal","message":"could not encode guest error"}`)
		return
	}
	result = bounded
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		if i == 0 {
			if char != '_' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
				return false
			}
			continue
		}
		if char != '_' &&
			(char < 'A' || char > 'Z') &&
			(char < 'a' || char > 'z') &&
			(char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func main() {}
