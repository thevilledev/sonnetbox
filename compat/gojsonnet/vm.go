// Package gojsonnet provides an opt-in migration surface shaped like the
// common github.com/google/go-jsonnet VM API.
package gojsonnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"

	nativejsonnet "github.com/google/go-jsonnet"
	"github.com/thevilledev/sonnetbox"
)

// VM stores mutable evaluation settings. Like go-jsonnet's VM, it must not be
// mutated or evaluated concurrently.
type VM struct {
	engine       *sonnetbox.Engine
	importer     sonnetbox.Importer
	extVars      map[string]string
	extCode      map[string]string
	tlaVars      map[string]string
	tlaCode      map[string]string
	capabilities map[string]sonnetbox.Capability
	traceOut     io.Writer

	// MaxStack lowers the engine stack ceiling when nonzero.
	MaxStack int
	// StringOutput requests unquoted top-level string output.
	StringOutput bool
	// OutputNewline controls the trailing output newline.
	OutputNewline bool
	// Limits lowers other engine ceilings for each evaluation.
	Limits sonnetbox.RequestLimits
}

// New creates a compatibility VM backed by one long-lived secure engine.
func New(engine *sonnetbox.Engine) (*VM, error) {
	if engine == nil {
		return nil, errors.New("sonnetbox compatibility VM requires an engine")
	}
	return &VM{
		engine:        engine,
		extVars:       make(map[string]string),
		extCode:       make(map[string]string),
		tlaVars:       make(map[string]string),
		tlaCode:       make(map[string]string),
		capabilities:  make(map[string]sonnetbox.Capability),
		traceOut:      os.Stderr,
		OutputNewline: true,
	}, nil
}

// RecommendedEngineConfig preserves go-jsonnet's default stack depth while
// leaving all other fields at sonnetbox defaults.
func RecommendedEngineConfig() sonnetbox.EngineConfig {
	return sonnetbox.EngineConfig{MaxStack: 500}
}

// Importer sets the request-scoped secure importer.
func (vm *VM) Importer(importer sonnetbox.Importer) {
	vm.importer = importer
}

// ExtVar binds a string external variable.
func (vm *VM) ExtVar(key, value string) {
	vm.extVars[key] = value
}

// ExtCode binds a Jsonnet-code external variable.
func (vm *VM) ExtCode(key, value string) {
	vm.extCode[key] = value
}

// ExtReset clears all external variables.
func (vm *VM) ExtReset() {
	vm.extVars = make(map[string]string)
	vm.extCode = make(map[string]string)
}

// TLAVar binds a string top-level argument.
func (vm *VM) TLAVar(key, value string) {
	vm.tlaVars[key] = value
}

// TLACode binds a Jsonnet-code top-level argument.
func (vm *VM) TLACode(key, value string) {
	vm.tlaCode[key] = value
}

// TLAReset clears all top-level arguments.
func (vm *VM) TLAReset() {
	vm.tlaVars = make(map[string]string)
	vm.tlaCode = make(map[string]string)
}

// NativeFunction registers a go-jsonnet native function as a pure capability.
func (vm *VM) NativeFunction(function *nativejsonnet.NativeFunction) {
	if function == nil {
		return
	}
	params := make([]string, len(function.Params))
	for index, param := range function.Params {
		params[index] = string(param)
	}
	vm.capabilities[function.Name] = sonnetbox.Capability{
		Params: params,
		Call: func(_ context.Context, args []any) (any, error) {
			return function.Func(args)
		},
	}
}

// SetTraceOut sets the destination for bounded std.trace output. A nil writer
// discards traces without crossing the sandbox boundary.
func (vm *VM) SetTraceOut(writer io.Writer) {
	vm.traceOut = writer
}

// EvaluateAnonymousSnippet mirrors go-jsonnet while accepting a context.
func (vm *VM) EvaluateAnonymousSnippet(
	ctx context.Context,
	filename string,
	source string,
) (string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeSingle, true)
	if err != nil {
		return "", err
	}
	return string(result.Output), nil
}

// EvaluateSnippet evaluates inline source with file-relative imports.
func (vm *VM) EvaluateSnippet(
	ctx context.Context,
	filename string,
	source string,
) (string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeSingle, false)
	if err != nil {
		return "", err
	}
	return string(result.Output), nil
}

// EvaluateFile loads and evaluates filename through the configured importer.
func (vm *VM) EvaluateFile(ctx context.Context, filename string) (string, error) {
	result, err := vm.evaluateFile(ctx, filename, sonnetbox.OutputModeSingle)
	if err != nil {
		return "", err
	}
	return string(result.Output), nil
}

// EvaluateAnonymousSnippetMulti mirrors go-jsonnet while accepting a context.
func (vm *VM) EvaluateAnonymousSnippetMulti(
	ctx context.Context,
	filename string,
	source string,
) (map[string]string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeMulti, true)
	if err != nil {
		return nil, err
	}
	return stringMap(result.Files), nil
}

// EvaluateSnippetMulti evaluates inline multi-file output with relative imports.
func (vm *VM) EvaluateSnippetMulti(
	ctx context.Context,
	filename string,
	source string,
) (map[string]string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeMulti, false)
	if err != nil {
		return nil, err
	}
	return stringMap(result.Files), nil
}

// EvaluateFileMulti loads filename and manifests filename/output pairs.
func (vm *VM) EvaluateFileMulti(
	ctx context.Context,
	filename string,
) (map[string]string, error) {
	result, err := vm.evaluateFile(ctx, filename, sonnetbox.OutputModeMulti)
	if err != nil {
		return nil, err
	}
	return stringMap(result.Files), nil
}

// EvaluateAnonymousSnippetStream mirrors go-jsonnet while accepting a context.
func (vm *VM) EvaluateAnonymousSnippetStream(
	ctx context.Context,
	filename string,
	source string,
) ([]string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeStream, true)
	if err != nil {
		return nil, err
	}
	return stringSlice(result.Documents), nil
}

// EvaluateSnippetStream evaluates inline stream output with relative imports.
func (vm *VM) EvaluateSnippetStream(
	ctx context.Context,
	filename string,
	source string,
) ([]string, error) {
	result, err := vm.evaluate(ctx, filename, source, sonnetbox.OutputModeStream, false)
	if err != nil {
		return nil, err
	}
	return stringSlice(result.Documents), nil
}

// EvaluateFileStream loads filename and manifests an ordered document stream.
func (vm *VM) EvaluateFileStream(
	ctx context.Context,
	filename string,
) ([]string, error) {
	result, err := vm.evaluateFile(ctx, filename, sonnetbox.OutputModeStream)
	if err != nil {
		return nil, err
	}
	return stringSlice(result.Documents), nil
}

func (vm *VM) evaluate(
	ctx context.Context,
	filename string,
	source string,
	mode sonnetbox.OutputMode,
	anonymous bool,
) (sonnetbox.Result, error) {
	request := vm.request(filename, source, mode)
	var result sonnetbox.Result
	var err error
	if anonymous {
		result, err = vm.engine.EvaluateAnonymous(ctx, request)
	} else {
		result, err = vm.engine.Evaluate(ctx, request)
	}
	if err != nil {
		return sonnetbox.Result{}, err
	}
	return vm.writeTrace(result)
}

func (vm *VM) evaluateFile(
	ctx context.Context,
	filename string,
	mode sonnetbox.OutputMode,
) (sonnetbox.Result, error) {
	request := vm.request(filename, "", mode)
	result, err := vm.engine.EvaluateFile(ctx, filename, request)
	if err != nil {
		return sonnetbox.Result{}, err
	}
	return vm.writeTrace(result)
}

func (vm *VM) request(
	filename string,
	source string,
	mode sonnetbox.OutputMode,
) sonnetbox.Request {
	limits := vm.Limits
	if vm.MaxStack != 0 {
		limits.MaxStack = vm.MaxStack
	}
	return sonnetbox.Request{
		Filename:            filename,
		Source:              source,
		ExtVars:             maps.Clone(vm.extVars),
		ExtCode:             maps.Clone(vm.extCode),
		TLAVars:             maps.Clone(vm.tlaVars),
		TLACode:             maps.Clone(vm.tlaCode),
		Importer:            vm.importer,
		Capabilities:        cloneCapabilities(vm.capabilities),
		Limits:              limits,
		OutputMode:          mode,
		StringOutput:        vm.StringOutput,
		OmitTrailingNewline: !vm.OutputNewline,
		CaptureTrace:        vm.traceOut != nil,
	}
}

func (vm *VM) writeTrace(
	result sonnetbox.Result,
) (sonnetbox.Result, error) {
	if vm.traceOut == nil || len(result.Trace) == 0 {
		return result, nil
	}
	if _, err := vm.traceOut.Write(result.Trace); err != nil {
		return sonnetbox.Result{}, fmt.Errorf("write Jsonnet trace: %w", err)
	}
	return result, nil
}

func cloneCapabilities(
	input map[string]sonnetbox.Capability,
) map[string]sonnetbox.Capability {
	output := make(map[string]sonnetbox.Capability, len(input))
	for name, capability := range input {
		capability.Params = slices.Clone(capability.Params)
		output[name] = capability
	}
	return output
}

func stringMap(input map[string][]byte) map[string]string {
	output := make(map[string]string, len(input))
	for name, value := range input {
		output[name] = string(value)
	}
	return output
}

func stringSlice(input [][]byte) []string {
	output := make([]string, len(input))
	for index, value := range input {
		output[index] = string(value)
	}
	return output
}

// ImporterAdapter serializes calls to a trusted go-jsonnet importer. It does
// not make FileImporter safe and refuses that importer explicitly.
type ImporterAdapter struct {
	mu       sync.Mutex
	importer nativejsonnet.Importer
}

// AdaptImporter wraps a trusted go-jsonnet importer for migration. Use
// sonnetbox.NewWorkspaceImporter instead of adapting FileImporter.
func AdaptImporter(importer nativejsonnet.Importer) (*ImporterAdapter, error) {
	if importer == nil {
		return nil, errors.New("go-jsonnet importer is nil")
	}
	if _, unsafe := importer.(*nativejsonnet.FileImporter); unsafe {
		return nil, errors.New(
			"go-jsonnet FileImporter is not an isolation boundary; use NewWorkspaceImporter",
		)
	}
	return &ImporterAdapter{importer: importer}, nil
}

// Import implements sonnetbox.Importer.
func (adapter *ImporterAdapter) Import(
	ctx context.Context,
	importedFrom string,
	importedPath string,
) (string, []byte, error) {
	if adapter == nil || adapter.importer == nil {
		return "", nil, errors.New("go-jsonnet importer adapter is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	adapter.mu.Lock()
	contents, foundAt, err := adapter.importer.Import(importedFrom, importedPath)
	adapter.mu.Unlock()
	if err != nil {
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	return foundAt, bytes.Clone(contents.Data()), nil
}
