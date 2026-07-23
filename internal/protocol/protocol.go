package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const ABIVersion uint32 = 2

const (
	EvalOK uint32 = iota
	EvalInvalidRequest
	EvalJsonnetError
	EvalHostError
	EvalLimit
	EvalInternal
)

const (
	HostOK uint32 = iota
	HostDenied
	HostHandlerFailure
	HostLimit
	HostCanceled
	HostMalformed
)

const (
	OperationResolveImport  uint32 = 1
	OperationCallCapability uint32 = 2
)

type Limits struct {
	MaxOutputBytes       uint32 `json:"max_output_bytes"`
	MaxStack             int    `json:"max_stack"`
	MaxHostRequestBytes  uint32 `json:"max_host_request_bytes"`
	MaxHostResponseBytes uint32 `json:"max_host_response_bytes"`
}

type CapabilityDescriptor struct {
	Params []string `json:"params"`
}

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

type ImportRequest struct {
	ImportedFrom string `json:"from"`
	ImportedPath string `json:"path"`
}

type ImportResponse struct {
	Canonical string `json:"canonical"`
	Content   []byte `json:"content_base64"`
}

type CapabilityRequest struct {
	Name string `json:"name"`
	Args []any  `json:"args"`
}

type CapabilityResponse struct {
	Value json.RawMessage `json:"value"`
}

type GuestError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func Pack(status, length uint32) uint64 {
	return uint64(status)<<32 | uint64(length)
}

func Unpack(v uint64) (status, length uint32) {
	return uint32(v >> 32), uint32(v)
}

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
