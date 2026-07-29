package sonnetbox

import (
	"context"
	"time"
)

// OutputMode selects how the top-level Jsonnet value is manifested.
type OutputMode uint8

const (
	// OutputModeSingle manifests one JSON value, or one unquoted string when
	// Request.StringOutput is set.
	OutputModeSingle OutputMode = iota
	// OutputModeMulti manifests a top-level object as filename/output pairs.
	OutputModeMulti
	// OutputModeStream manifests a top-level array as a sequence of documents.
	OutputModeStream
)

// EngineConfig sets resource ceilings for an Engine. A zero field selects the
// documented default for that field.
//
// EngineConfig holds only policy values, so it round-trips through JSON and
// can be loaded from an operator-supplied policy file. Use
// [DefaultEngineConfig] to discover the defaults and [Ceilings] to discover
// the maximum value each field accepts.
type EngineConfig struct {
	// MaxMemoryBytes limits each guest's linear memory. It must be a multiple
	// of 64 KiB.
	MaxMemoryBytes uint64 `json:"max_memory_bytes"`
	// MaxWasmStackBytes limits the compiled WebAssembly call stack. It is an
	// engine-wide limit and has no effect when using WithInterpreter.
	MaxWasmStackBytes uint64 `json:"max_wasm_stack_bytes"`
	// MaxFuel limits deterministic WebAssembly instruction work during one
	// evaluation.
	MaxFuel uint64 `json:"max_fuel"`
	// MaxSourceBytes limits Request.Source in bytes.
	MaxSourceBytes uint32 `json:"max_source_bytes"`
	// MaxOutputBytes limits the rendered result in bytes.
	MaxOutputBytes uint32 `json:"max_output_bytes"`
	// MaxStack limits go-jsonnet interpreter stack depth.
	MaxStack int `json:"max_stack"`
	// MaxImports limits import resolutions during one evaluation.
	MaxImports uint32 `json:"max_imports"`
	// MaxImportBytes limits one imported file in bytes.
	MaxImportBytes uint32 `json:"max_import_bytes"`
	// MaxTotalImportBytes limits all imported content during one evaluation.
	MaxTotalImportBytes uint64 `json:"max_total_import_bytes"`
	// MaxCapabilityCalls limits native capability calls during one evaluation.
	MaxCapabilityCalls uint32 `json:"max_capability_calls"`
	// MaxHostRequestBytes limits encoded requests crossing from guest to host
	// and the encoded evaluation request crossing from host to guest.
	MaxHostRequestBytes uint32 `json:"max_host_request_bytes"`
	// MaxHostResponseBytes limits encoded responses crossing from host to
	// guest. Import content is base64-encoded within this limit. Nonzero values
	// must be at least 256 bytes.
	MaxHostResponseBytes uint32 `json:"max_host_response_bytes"`
	// MaxTraceBytes limits captured std.trace output during one evaluation.
	MaxTraceBytes uint32 `json:"max_trace_bytes"`
	// MaxConcurrentEvaluations limits active guest instances. Additional
	// evaluations wait for capacity while honoring context cancellation.
	MaxConcurrentEvaluations uint32 `json:"max_concurrent_evaluations"`
}

// RequestLimits lowers resource limits for one evaluation. A zero field
// inherits the corresponding EngineConfig ceiling. A nonzero field cannot
// exceed that ceiling.
type RequestLimits struct {
	// MaxFuel lowers EngineConfig.MaxFuel.
	MaxFuel uint64 `json:"max_fuel,omitempty"`
	// MaxSourceBytes lowers EngineConfig.MaxSourceBytes.
	MaxSourceBytes uint32 `json:"max_source_bytes,omitempty"`
	// MaxOutputBytes lowers EngineConfig.MaxOutputBytes.
	MaxOutputBytes uint32 `json:"max_output_bytes,omitempty"`
	// MaxStack lowers EngineConfig.MaxStack.
	MaxStack int `json:"max_stack,omitempty"`
	// MaxImports lowers EngineConfig.MaxImports.
	MaxImports uint32 `json:"max_imports,omitempty"`
	// MaxImportBytes lowers EngineConfig.MaxImportBytes.
	MaxImportBytes uint32 `json:"max_import_bytes,omitempty"`
	// MaxTotalImportBytes lowers EngineConfig.MaxTotalImportBytes.
	MaxTotalImportBytes uint64 `json:"max_total_import_bytes,omitempty"`
	// MaxCapabilityCalls lowers EngineConfig.MaxCapabilityCalls.
	MaxCapabilityCalls uint32 `json:"max_capability_calls,omitempty"`
	// MaxHostRequestBytes lowers EngineConfig.MaxHostRequestBytes.
	MaxHostRequestBytes uint32 `json:"max_host_request_bytes,omitempty"`
	// MaxHostResponseBytes lowers EngineConfig.MaxHostResponseBytes.
	MaxHostResponseBytes uint32 `json:"max_host_response_bytes,omitempty"`
	// MaxTraceBytes lowers EngineConfig.MaxTraceBytes.
	MaxTraceBytes uint32 `json:"max_trace_bytes,omitempty"`
}

// Importer resolves a Jsonnet import without granting guest filesystem access.
// Implementations are trusted host code and must be safe for concurrent calls
// from separate evaluations. They should return errors wrapping
// ErrImportDenied for paths that are absent or rejected by policy.
type Importer interface {
	// Import resolves importedPath relative to importedFrom. It returns a
	// canonical virtual path and its content. The content returned for a
	// canonical path must remain stable during an evaluation.
	Import(
		ctx context.Context,
		importedFrom string,
		importedPath string,
	) (canonicalPath string, content []byte, err error)
}

// Capability defines a pure Jsonnet native function. Call may execute zero,
// one, or multiple times and may be called concurrently by separate
// evaluations.
type Capability struct {
	// Params lists the Jsonnet function's parameter names.
	Params []string
	// Call executes trusted host code with JSON-compatible arguments and must
	// honor context cancellation.
	Call func(context.Context, []any) (any, error)
}

// Request describes one isolated Jsonnet evaluation.
type Request struct {
	// Filename is the virtual, canonical filename used in diagnostics and
	// relative imports. An empty value selects "snippet.jsonnet".
	Filename string
	// Source is the adversarial Jsonnet program to evaluate.
	Source string
	// ExtVars supplies string external variables.
	ExtVars map[string]string
	// ExtCode supplies Jsonnet-code external variables.
	ExtCode map[string]string
	// TLAVars supplies string top-level arguments.
	TLAVars map[string]string
	// TLACode supplies Jsonnet-code top-level arguments.
	TLACode map[string]string
	// Importer resolves virtual imports. A nil Importer denies all imports.
	Importer Importer
	// Capabilities exposes only these request-scoped native functions.
	Capabilities map[string]Capability
	// Limits optionally lowers this evaluation's resource limits.
	Limits RequestLimits
	// OutputMode selects single, multi-file, or stream manifestation.
	OutputMode OutputMode
	// StringOutput returns a top-level Jsonnet string without JSON quoting.
	// It applies to single and multi-file output.
	StringOutput bool
	// OmitTrailingNewline disables go-jsonnet's default output newline.
	OmitTrailingNewline bool
	// CaptureTrace returns bounded std.trace output in Result.Trace.
	CaptureTrace bool
}

// EvaluationStats reports the work an evaluation performed, including one that
// failed after the guest reported a status. FuelConsumed is deterministic for
// the same guest and input; durations and other host-observed counters are
// diagnostic.
type EvaluationStats struct {
	// QueueDuration is the time spent waiting for an engine concurrency slot.
	QueueDuration time.Duration
	// ExecutionDuration is the time from acquiring a slot through decoding the
	// completed guest result.
	ExecutionDuration time.Duration
	// FuelConsumed is the deterministic WebAssembly instruction work used.
	FuelConsumed uint64
	// ImportResolutions is the number of import requests made by the guest.
	ImportResolutions uint32
	// ImportBytes is the cumulative size of imported content.
	ImportBytes uint64
	// CapabilityCalls is the number of native capability calls.
	CapabilityCalls uint32
	// TraceBytes is the number of captured std.trace bytes.
	TraceBytes uint32
	// TraceTruncated reports whether trace output exceeded its configured
	// limit.
	TraceTruncated bool
}

// Result is a completed Jsonnet evaluation. The request's OutputMode selects
// Output, Files, or Documents for the manifested value.
//
// A failed evaluation still returns Trace and Stats alongside its error when
// Request.CaptureTrace is set and the guest reached the point of reporting a
// status. The manifested value is empty in that case. Nothing is recoverable
// when the guest is trapped by a fuel, memory, or deadline backstop, because
// no further guest call can succeed.
type Result struct {
	// Output contains single-mode rendered JSON or StringOutput bytes.
	Output []byte
	// Files contains multi-mode rendered outputs keyed by filename.
	Files map[string][]byte
	// Documents contains stream-mode rendered documents in source order.
	Documents [][]byte
	// Trace contains captured std.trace output.
	Trace []byte
	// Imports lists the canonical paths the importer served, deduplicated and
	// in resolution order. It reports what this evaluation actually resolved,
	// not what a static parse of the source could reach, so a lazily unused
	// import is absent and a conditional import appears only when the taken
	// branch needed it. The list is bounded by the MaxImports ceiling.
	Imports []string
	// Stats reports bounded host-observed evaluation work.
	Stats EvaluationStats
}

// VersionInfo identifies the evaluator and host/guest ABI.
type VersionInfo struct {
	// Jsonnet is the embedded go-jsonnet semantic version.
	Jsonnet string
	// ABI is the sonnetbox host/guest protocol version.
	ABI uint32
}
