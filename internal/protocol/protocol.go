// Package protocol defines the private host-to-guest wire protocol.
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// ABIVersion is the current host-to-guest ABI version.
const ABIVersion uint32 = 7

// InputMode selects whether source is supplied inline or loaded through the
// importer.
type InputMode uint8

const (
	// InputSnippet evaluates source supplied in EvaluationRequest.Source.
	InputSnippet InputMode = iota
	// InputFile loads EvaluationRequest.Filename through the importer.
	InputFile
	// InputAnonymous uses the filename only for diagnostics.
	InputAnonymous
)

// OutputMode selects the guest manifestation shape.
type OutputMode uint8

const (
	// OutputSingle manifests one value.
	OutputSingle OutputMode = iota
	// OutputMulti manifests filename/output pairs.
	OutputMulti
	// OutputStream manifests an ordered document sequence.
	OutputStream
)

const (
	// EvalOK reports a successful guest evaluation.
	EvalOK uint32 = iota
	// EvalInvalidRequest reports a request rejected by the guest.
	EvalInvalidRequest
	// EvalJsonnetError reports a Jsonnet evaluation error.
	EvalJsonnetError
	// EvalHostError reports an error from a host callback.
	EvalHostError
	// EvalLimit reports a guest-enforced resource limit.
	EvalLimit
	// EvalInternal reports an unexpected guest failure.
	EvalInternal
)

const (
	// HostOK reports a successful host callback.
	HostOK uint32 = iota
	// HostDenied reports a denied host request.
	HostDenied
	// HostHandlerFailure reports a trusted handler failure.
	HostHandlerFailure
	// HostLimit reports a host-enforced resource limit.
	HostLimit
	// HostCanceled reports context cancellation.
	HostCanceled
	// HostMalformed reports a malformed host request.
	HostMalformed
)

const (
	// OperationResolveImport asks the host to resolve a virtual import.
	OperationResolveImport uint32 = 1
	// OperationCallCapability asks the host to invoke a native capability.
	OperationCallCapability uint32 = 2
)

// Limits contains the guest-enforced request limits.
type Limits struct {
	MaxOutputBytes       uint32 `json:"max_output_bytes"`
	MaxStack             int    `json:"max_stack"`
	MaxHostRequestBytes  uint32 `json:"max_host_request_bytes"`
	MaxHostResponseBytes uint32 `json:"max_host_response_bytes"`
	MaxTraceBytes        uint32 `json:"max_trace_bytes"`
}

// CapabilityDescriptor describes a registered native capability.
type CapabilityDescriptor struct {
	Params []string `json:"params"`
}

// EvaluationRequest is the encoded host-to-guest evaluation request.
type EvaluationRequest struct {
	Filename      string                          `json:"filename"`
	Source        string                          `json:"source"`
	InputMode     InputMode                       `json:"input_mode,omitempty"`
	OutputMode    OutputMode                      `json:"output_mode,omitempty"`
	ExtVars       map[string]string               `json:"ext_vars,omitempty"`
	ExtCode       map[string]string               `json:"ext_code,omitempty"`
	TLAVars       map[string]string               `json:"tla_vars,omitempty"`
	TLACode       map[string]string               `json:"tla_code,omitempty"`
	Capabilities  map[string]CapabilityDescriptor `json:"capabilities,omitempty"`
	StringOutput  bool                            `json:"string_output,omitempty"`
	OutputNewline bool                            `json:"output_newline"`
	CaptureTrace  bool                            `json:"capture_trace,omitempty"`
	Limits        Limits                          `json:"limits"`
}

// ImportRequest is a guest request to resolve a virtual import.
type ImportRequest struct {
	ImportedFrom string `json:"from"`
	ImportedPath string `json:"path"`
}

// ImportResponse is a successful host import response.
type ImportResponse struct {
	Canonical string `json:"canonical"`
	Content   []byte `json:"content_base64"`
}

// CapabilityRequest is a guest request to invoke a native capability.
type CapabilityRequest struct {
	Name string `json:"name"`
	Args []any  `json:"args"`
}

// CapabilityResponse is a successful host capability response.
type CapabilityResponse struct {
	Value json.RawMessage `json:"value"`
}

// GuestError is the guest's structured error response.
type GuestError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Limit   uint64 `json:"limit,omitempty"`
	Actual  uint64 `json:"actual,omitempty"`
}

// Pack combines a status and payload length into an ABI return value.
func Pack(status, length uint32) uint64 {
	return uint64(status)<<32 | uint64(length)
}

// Unpack splits an ABI return value into its status and payload length.
func Unpack(v uint64) (status, length uint32) {
	return uint32(v >> 32), uint32(v) //nolint:gosec // ABI decoding intentionally selects each 32-bit half.
}

// DecodeJSON decodes exactly one strict JSON value.
func DecodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
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

// EncodeMultiOutput encodes filename/output pairs deterministically.
func EncodeMultiOutput(files map[string]string) ([]byte, error) {
	if uint64(len(files)) > math.MaxUint32 {
		return nil, errors.New("too many multi-file outputs")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(names))) //nolint:gosec // length is bounded above.
	for _, name := range names {
		var err error
		out, err = appendFramedString(out, name)
		if err != nil {
			return nil, err
		}
		out, err = appendFramedString(out, files[name])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DecodeMultiOutput decodes filename/output pairs produced by EncodeMultiOutput.
func DecodeMultiOutput(data []byte) (map[string][]byte, error) {
	count, rest, err := consumeUint32(data)
	if err != nil {
		return nil, err
	}
	if uint64(count) > uint64(len(rest))/8 {
		return nil, errors.New("multi-file output count exceeds payload")
	}
	files := make(map[string][]byte, int(count))
	for range count {
		var name []byte
		name, rest, err = consumeBytes(rest)
		if err != nil {
			return nil, err
		}
		var output []byte
		output, rest, err = consumeBytes(rest)
		if err != nil {
			return nil, err
		}
		key := string(name)
		if _, exists := files[key]; exists {
			return nil, fmt.Errorf("duplicate multi-file output %q", key)
		}
		files[key] = append([]byte(nil), output...)
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing multi-file output bytes")
	}
	return files, nil
}

// EncodeStreamOutput encodes stream documents in source order.
func EncodeStreamOutput(documents []string) ([]byte, error) {
	if uint64(len(documents)) > math.MaxUint32 {
		return nil, errors.New("too many stream documents")
	}
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(documents))) //nolint:gosec // length is bounded above.
	for _, document := range documents {
		var err error
		out, err = appendFramedString(out, document)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DecodeStreamOutput decodes documents produced by EncodeStreamOutput.
func DecodeStreamOutput(data []byte) ([][]byte, error) {
	count, rest, err := consumeUint32(data)
	if err != nil {
		return nil, err
	}
	if uint64(count) > uint64(len(rest))/4 {
		return nil, errors.New("stream output count exceeds payload")
	}
	documents := make([][]byte, 0, int(count))
	for range count {
		var document []byte
		document, rest, err = consumeBytes(rest)
		if err != nil {
			return nil, err
		}
		documents = append(documents, append([]byte(nil), document...))
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing stream output bytes")
	}
	return documents, nil
}

func appendFramedString(out []byte, value string) ([]byte, error) {
	if uint64(len(value)) > math.MaxUint32 {
		return nil, errors.New("output component is too large")
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value))) //nolint:gosec // length is bounded above.
	return append(out, value...), nil
}

func consumeUint32(data []byte) (uint32, []byte, error) {
	if len(data) < 4 {
		return 0, nil, errors.New("truncated output frame")
	}
	return binary.LittleEndian.Uint32(data[:4]), data[4:], nil
}

func consumeBytes(data []byte) ([]byte, []byte, error) {
	length, rest, err := consumeUint32(data)
	if err != nil {
		return nil, nil, err
	}
	if uint64(length) > uint64(len(rest)) {
		return nil, nil, errors.New("output frame length exceeds payload")
	}
	return rest[:length], rest[length:], nil
}
