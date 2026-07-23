package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const ABIVersion uint32 = 1

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
	HostLimit
	HostCanceled
	HostHandlerFailure
	HostMalformed
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
	Filename     string                          `json:"filename"`
	Source       string                          `json:"source"`
	ExtVars      map[string]string               `json:"ext_vars,omitempty"`
	ExtCode      map[string]string               `json:"ext_code,omitempty"`
	TLAVars      map[string]string               `json:"tla_vars,omitempty"`
	TLACode      map[string]string               `json:"tla_code,omitempty"`
	Capabilities map[string]CapabilityDescriptor `json:"capabilities,omitempty"`
	Limits       Limits                          `json:"limits"`
}

type ImportRequest struct {
	ImportedFrom string `json:"imported_from"`
	ImportedPath string `json:"imported_path"`
}

type CapabilityRequest struct {
	Name string `json:"name"`
	Args []any  `json:"args"`
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

func EncodeImportResponse(canonical string, content []byte) ([]byte, error) {
	if uint64(len(canonical))+uint64(len(content))+4 > math.MaxUint32 {
		return nil, errors.New("import response exceeds uint32")
	}
	out := make([]byte, 4+len(canonical)+len(content))
	binary.LittleEndian.PutUint32(out, uint32(len(canonical)))
	copy(out[4:], canonical)
	copy(out[4+len(canonical):], content)
	return out, nil
}

func DecodeImportResponse(payload []byte) (string, []byte, error) {
	if len(payload) < 4 {
		return "", nil, errors.New("import response is shorter than header")
	}
	nameLen := binary.LittleEndian.Uint32(payload)
	if uint64(nameLen) > uint64(len(payload)-4) {
		return "", nil, fmt.Errorf("canonical path length %d exceeds payload", nameLen)
	}
	offset := 4 + int(nameLen)
	return string(payload[4:offset]), payload[offset:], nil
}
