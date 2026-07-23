// Package protocol defines the private host-to-guest wire protocol.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ABIVersion is the current host-to-guest ABI version.
const ABIVersion uint32 = 2

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
}

// CapabilityDescriptor describes a registered native capability.
type CapabilityDescriptor struct {
	Params []string `json:"params"`
}

// EvaluationRequest is the encoded host-to-guest evaluation request.
type EvaluationRequest struct {
	Filename      string                          `json:"filename"`
	Source        string                          `json:"source"`
	ExtVars       map[string]string               `json:"ext_vars,omitempty"`
	ExtCode       map[string]string               `json:"ext_code,omitempty"`
	TLAVars       map[string]string               `json:"tla_vars,omitempty"`
	TLACode       map[string]string               `json:"tla_code,omitempty"`
	Capabilities  map[string]CapabilityDescriptor `json:"capabilities,omitempty"`
	StringOutput  bool                            `json:"string_output,omitempty"`
	OutputNewline bool                            `json:"output_newline"`
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
