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

// EngineConfig sets process-wide ceilings for an Engine. A zero field selects
// the documented default for that field.
type EngineConfig struct {
	// MaxMemoryBytes limits each guest's linear memory. It must be a multiple
	// of 64 KiB.
	MaxMemoryBytes uint64
	// MaxSourceBytes limits Request.Source in bytes.
	MaxSourceBytes uint32
	// MaxOutputBytes limits the rendered result in bytes.
	MaxOutputBytes uint32
	// MaxStack limits go-jsonnet interpreter stack depth.
	MaxStack int
	// MaxImports limits import resolutions during one evaluation.
	MaxImports uint32
	// MaxImportBytes limits one imported file in bytes.
	MaxImportBytes uint32
	// MaxTotalImportBytes limits all imported content during one evaluation.
	MaxTotalImportBytes uint64
	// MaxCapabilityCalls limits native capability calls during one evaluation.
	MaxCapabilityCalls uint32
	// MaxHostRequestBytes limits encoded requests crossing from guest to host
	// and the encoded evaluation request crossing from host to guest.
	MaxHostRequestBytes uint32
	// MaxHostResponseBytes limits encoded responses crossing from host to
	// guest. Import content is base64-encoded within this limit. Nonzero values
	// must be at least 256 bytes.
	MaxHostResponseBytes uint32
	// MaxTraceBytes limits captured std.trace output during one evaluation.
	MaxTraceBytes uint32
	// MaxConcurrentEvaluations limits active guest instances. Additional
	// evaluations wait for capacity while honoring context cancellation.
	MaxConcurrentEvaluations uint32
}

// RequestLimits lowers resource limits for one evaluation. Zero fields inherit
// their EngineConfig ceiling.
type RequestLimits struct {
	MaxSourceBytes       uint32
	MaxOutputBytes       uint32
	MaxStack             int
	MaxImports           uint32
	MaxImportBytes       uint32
	MaxTotalImportBytes  uint64
	MaxCapabilityCalls   uint32
	MaxHostRequestBytes  uint32
	MaxHostResponseBytes uint32
	MaxTraceBytes        uint32
}

// Importer resolves a Jsonnet import without granting guest filesystem access.
// Implementations are trusted host code and must be safe for concurrent calls
// from separate evaluations.
type Importer interface {
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

// EvaluationStats reports host-observed work for a successful evaluation.
type EvaluationStats struct {
	QueueDuration     time.Duration
	ExecutionDuration time.Duration
	ImportResolutions uint32
	ImportBytes       uint64
	CapabilityCalls   uint32
	TraceBytes        uint32
	TraceTruncated    bool
}

// Result is a completed Jsonnet evaluation.
type Result struct {
	// Output contains single-mode rendered JSON or StringOutput bytes.
	Output []byte
	// Files contains multi-mode rendered outputs keyed by filename.
	Files map[string][]byte
	// Documents contains stream-mode rendered documents in source order.
	Documents [][]byte
	// Trace contains captured std.trace output.
	Trace []byte
	// Stats reports bounded host-observed evaluation work.
	Stats EvaluationStats
}

// VersionInfo identifies the evaluator and private host/guest ABI.
type VersionInfo struct {
	Jsonnet string
	ABI     uint32
}
