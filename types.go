package securejsonnet

import "context"

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
	// OutputMode selects single, multi-file, or stream manifestation.
	OutputMode OutputMode
	// StringOutput returns a top-level Jsonnet string without JSON quoting.
	// It applies to single and multi-file output.
	StringOutput bool
	// OmitTrailingNewline disables go-jsonnet's default output newline.
	OmitTrailingNewline bool
}

// Result is a completed Jsonnet evaluation.
type Result struct {
	// Output contains single-mode rendered JSON or StringOutput bytes.
	Output []byte
	// Files contains multi-mode rendered outputs keyed by filename.
	Files map[string][]byte
	// Documents contains stream-mode rendered documents in source order.
	Documents [][]byte
}
