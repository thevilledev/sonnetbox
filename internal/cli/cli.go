// Package cli implements the secure sonnetbox command-line interface.
package cli

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thevilledev/sonnetbox"
)

const (
	defaultTimeout = 5 * time.Second
	// usageColumn aligns flag descriptions in the help text.
	usageColumn = 30
	// maxMountAttempts bounds the search for a free virtual library-path name.
	maxMountAttempts = 100
)

var releaseVersion string

// refusedOptions names upstream jsonnet options sonnetbox deliberately does not
// implement, so a migrating operator reads the reason instead of a bare
// unrecognized-option error.
var refusedOptions = map[string]string{
	"-t":                  "stack-trace cropping is not supported",
	"--max-trace":         "stack-trace cropping is not supported",
	"--gc-min-objects":    "the sandbox bounds guest memory; see --max-memory",
	"--gc-growth-trigger": "the sandbox bounds guest memory; see --max-memory",
	"--ext-str-env":       "sonnetbox never reads a value from the environment",
	"--ext-code-env":      "sonnetbox never reads a value from the environment",
}

// bindingKind selects which request field a deferred variable file fills.
type bindingKind int

const (
	bindingExtStr bindingKind = iota
	bindingExtCode
	bindingTLAStr
	bindingTLACode
)

// varFile records an operator-named host file whose contents become a variable
// value. The read is deferred until the source limit is known.
type varFile struct {
	kind bindingKind
	name string
	path string
}

type config struct {
	input               string
	exec                bool
	jpaths              []string
	varFiles            []varFile
	outputFile          string
	multiDir            string
	createOutputDirs    bool
	stream              bool
	stringOutput        bool
	omitTrailingNewline bool
	extVars             map[string]string
	extCode             map[string]string
	tlaVars             map[string]string
	tlaCode             map[string]string
	root                string
	timeout             time.Duration
	policyFile          string
	policyOverrides     []policyOverride
	printPolicy         bool
	cacheDir            string
	noCache             bool
	errorFormat         string
	help                bool
	version             bool
}

// Run executes the command with explicit process dependencies and returns its
// exit status.
func Run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	cfg, err := parseArgs(args)
	if err != nil {
		// A usage mistake is a human problem, so it always gets the human form.
		_, _ = fmt.Fprintf(stderr, "sonnetbox: %v\n\n", err)
		writeUsage(stderr)
		return exitFailure
	}
	if cfg.help {
		writeUsage(stdout)
		return exitSuccess
	}
	if cfg.version {
		writeVersion(stdout)
		return exitSuccess
	}
	if cfg.printPolicy {
		if err := writePolicy(stdout, cfg); err != nil {
			return reportError(stderr, cfg.errorFormat, err)
		}
		return exitSuccess
	}
	warnIgnoredEnvironment(stderr)
	if err := execute(ctx, cfg, stdin, stdout, stderr); err != nil {
		return reportError(stderr, cfg.errorFormat, err)
	}
	return exitSuccess
}

// warnIgnoredEnvironment reports process state that upstream jsonnet would act
// on. Silently ignoring it would leave an operator debugging imports that were
// never granted.
func warnIgnoredEnvironment(stderr io.Writer) {
	if os.Getenv("JSONNET_PATH") == "" {
		return
	}
	_, _ = fmt.Fprintln(
		stderr,
		"sonnetbox: JSONNET_PATH is ignored; grant library paths with --root and -J",
	)
}

func parseArgs(args []string) (config, error) {
	cfg := config{
		timeout:     defaultTimeout,
		errorFormat: errorFormatText,
		extVars:     make(map[string]string),
		extCode:     make(map[string]string),
		tlaVars:     make(map[string]string),
		tlaCode:     make(map[string]string),
	}
	var positional []string
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}
		if !options || arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		name, attached, hasAttached := strings.Cut(arg, "=")
		next := func() (string, error) {
			if hasAttached {
				return attached, nil
			}
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			return args[i], nil
		}
		switch name {
		case "-h", "--help":
			cfg.help = true
		case "-v", "--version":
			cfg.version = true
		case "-e", "--exec":
			cfg.exec = true
		case "-J", "--jpath":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("JPath is empty")
			}
			cfg.jpaths = append(cfg.jpaths, value)
		case "-o", "--output-file":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("output file is empty")
			}
			cfg.outputFile = value
		case "-m", "--multi":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("multi-file output directory is empty")
			}
			cfg.multiDir = value
		case "-c", "--create-output-dirs":
			cfg.createOutputDirs = true
		case "-y", "--yaml-stream":
			cfg.stream = true
		case "-S", "--string":
			cfg.stringOutput = true
		case "--no-trailing-newline":
			cfg.omitTrailingNewline = true
		case "-s", "--max-stack":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			stack, err := strconv.Atoi(value)
			if err != nil || stack < 1 {
				return config{}, fmt.Errorf("invalid max stack %q", value)
			}
			cfg.policyOverrides = append(
				cfg.policyOverrides,
				func(policy *sonnetbox.EngineConfig) { policy.MaxStack = stack },
			)
		case "-V", "--ext-str":
			if err := parseBinding(next, cfg.extVars); err != nil {
				return config{}, fmt.Errorf("external string: %w", err)
			}
		case "--ext-code":
			if err := parseBinding(next, cfg.extCode); err != nil {
				return config{}, fmt.Errorf("external code: %w", err)
			}
		case "-A", "--tla-str":
			if err := parseBinding(next, cfg.tlaVars); err != nil {
				return config{}, fmt.Errorf("top-level string: %w", err)
			}
		case "--tla-code":
			if err := parseBinding(next, cfg.tlaCode); err != nil {
				return config{}, fmt.Errorf("top-level code: %w", err)
			}
		case "--ext-str-file":
			if err := parseVarFile(next, &cfg, bindingExtStr); err != nil {
				return config{}, fmt.Errorf("external string file: %w", err)
			}
		case "--ext-code-file":
			if err := parseVarFile(next, &cfg, bindingExtCode); err != nil {
				return config{}, fmt.Errorf("external code file: %w", err)
			}
		case "--tla-str-file":
			if err := parseVarFile(next, &cfg, bindingTLAStr); err != nil {
				return config{}, fmt.Errorf("top-level string file: %w", err)
			}
		case "--tla-code-file":
			if err := parseVarFile(next, &cfg, bindingTLACode); err != nil {
				return config{}, fmt.Errorf("top-level code file: %w", err)
			}
		case "--root":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("workspace root is empty")
			}
			cfg.root = value
		case "--timeout":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			timeout, err := time.ParseDuration(value)
			if err != nil || timeout <= 0 {
				return config{}, fmt.Errorf("invalid timeout %q: must be a positive duration", value)
			}
			cfg.timeout = timeout
		case "--policy":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("policy file is empty")
			}
			cfg.policyFile = value
		case "--print-policy":
			cfg.printPolicy = true
		case "--cache-dir":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value == "" {
				return config{}, errors.New("cache directory is empty")
			}
			cfg.cacheDir = value
		case "--no-cache":
			cfg.noCache = true
		case "--error-format":
			value, err := next()
			if err != nil {
				return config{}, err
			}
			if value != errorFormatText && value != errorFormatJSON {
				return config{}, fmt.Errorf(
					"invalid error format %q: want %q or %q",
					value, errorFormatText, errorFormatJSON,
				)
			}
			cfg.errorFormat = value
		default:
			handled, err := applyPolicyFlag(&cfg, name, next)
			if err != nil {
				return config{}, err
			}
			if !handled {
				if reason, refused := refusedOptions[name]; refused {
					return config{}, fmt.Errorf("%s is not supported: %s", name, reason)
				}
				return config{}, fmt.Errorf("unrecognized option %q", arg)
			}
		}
	}

	if cfg.cacheDir != "" && cfg.noCache {
		return config{}, errors.New("--cache-dir and --no-cache cannot be combined")
	}
	if cfg.help || cfg.version || cfg.printPolicy {
		return cfg, nil
	}
	if len(positional) != 1 {
		return config{}, errors.New("exactly one filename, '-', or Jsonnet expression is required")
	}
	cfg.input = positional[0]
	if cfg.stream && cfg.multiDir != "" {
		return config{}, errors.New("--yaml-stream and --multi cannot be combined")
	}
	if cfg.stream && cfg.omitTrailingNewline {
		return config{}, errors.New("--no-trailing-newline cannot be used with --yaml-stream")
	}
	if cfg.createOutputDirs && cfg.outputFile == "" && cfg.multiDir == "" {
		return config{}, errors.New("--create-output-dirs requires --output-file or --multi")
	}
	return cfg, nil
}

func parseBinding(
	next func() (string, error),
	target map[string]string,
) error {
	value, err := next()
	if err != nil {
		return err
	}
	name, content, ok := strings.Cut(value, "=")
	if !ok && value != "" {
		// Upstream jsonnet reads a bare name from the environment. Doing so
		// would make the evaluated value depend on ambient process state, so
		// point at the explicit form instead.
		return fmt.Errorf(
			"%q is not in name=value form: sonnetbox never infers a value from "+
				"the environment; pass it explicitly as %s=$%s",
			value, value, value,
		)
	}
	if !ok || name == "" {
		return fmt.Errorf("%q is not in name=value form", value)
	}
	target[name] = content
	return nil
}

// parseVarFile records a name=file binding. The file is an explicit operator
// grant on the command line, so the host reads it directly rather than routing
// it through the sandbox importer as upstream jsonnet does.
func parseVarFile(
	next func() (string, error),
	cfg *config,
	kind bindingKind,
) error {
	value, err := next()
	if err != nil {
		return err
	}
	name, filename, ok := strings.Cut(value, "=")
	if !ok || name == "" || filename == "" {
		return fmt.Errorf("%q is not in name=file form", value)
	}
	cfg.varFiles = append(cfg.varFiles, varFile{kind: kind, name: name, path: filename})
	return nil
}

// loadVarFiles reads every deferred variable file once the source limit that
// bounds it is known.
func loadVarFiles(cfg config, limit uint32) error {
	for _, file := range cfg.varFiles {
		content, err := readVarFile(file.path, int64(limit))
		if err != nil {
			return err
		}
		switch file.kind {
		case bindingExtStr:
			cfg.extVars[file.name] = content
		case bindingExtCode:
			cfg.extCode[file.name] = content
		case bindingTLAStr:
			cfg.tlaVars[file.name] = content
		case bindingTLACode:
			cfg.tlaCode[file.name] = content
		}
	}
	return nil
}

func readVarFile(filename string, limit int64) (string, error) {
	// The path is an explicit operator grant, never a Jsonnet-supplied name.
	file, err := os.Open(filename) //nolint:gosec // See the operator-grant comment above.
	if err != nil {
		return "", fmt.Errorf("open variable file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", fmt.Errorf("read variable file %q: %w", filename, err)
	}
	if int64(len(content)) > limit {
		return "", fmt.Errorf(
			"variable file %q exceeds source limit of %d bytes",
			filename, limit,
		)
	}
	return string(content), nil
}

func execute(
	ctx context.Context,
	cfg config,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if ctx == nil {
		return errors.New("context is nil")
	}
	policy, err := resolvePolicy(cfg)
	if err != nil {
		return err
	}
	if err := loadVarFiles(cfg, policy.MaxSourceBytes); err != nil {
		return err
	}
	importer, filename, source, err := prepareInput(ctx, cfg, stdin, policy.MaxSourceBytes)
	if err != nil {
		return err
	}
	if importer != nil {
		defer func() {
			_ = importer.Close()
		}()
	}

	cache, err := openCompilationCache(cfg)
	if err != nil {
		return err
	}
	var options []sonnetbox.Option
	if cache != nil {
		defer cache.Close(context.WithoutCancel(ctx)) //nolint:errcheck // the evaluation result is authoritative.
		options = append(options, sonnetbox.WithCompilationCache(cache))
	}
	engine, err := sonnetbox.NewEngine(ctx, policy, options...)
	if err != nil {
		return fmt.Errorf("initialize evaluator: %w", err)
	}
	defer engine.Close(context.WithoutCancel(ctx)) //nolint:errcheck // the evaluation result is authoritative.

	evalCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	var requestImporter sonnetbox.Importer
	if importer != nil {
		requestImporter = importer
	}
	request := sonnetbox.Request{
		Filename:            filename,
		Source:              source,
		ExtVars:             cfg.extVars,
		ExtCode:             cfg.extCode,
		TLAVars:             cfg.tlaVars,
		TLACode:             cfg.tlaCode,
		Importer:            requestImporter,
		OutputMode:          outputMode(cfg),
		StringOutput:        cfg.stringOutput,
		OmitTrailingNewline: cfg.omitTrailingNewline,
		CaptureTrace:        true,
	}
	var result sonnetbox.Result
	if cfg.exec || cfg.input == "-" {
		result, err = engine.EvaluateAnonymous(evalCtx, request)
	} else {
		request.Source = ""
		result, err = engine.EvaluateFile(evalCtx, filename, request)
	}
	if err != nil {
		return err
	}
	if len(result.Trace) > 0 {
		if _, err := stderr.Write(result.Trace); err != nil {
			return fmt.Errorf("write trace: %w", err)
		}
	}
	if result.Stats.TraceTruncated {
		if _, err := fmt.Fprintln(stderr, "sonnetbox: std.trace output truncated"); err != nil {
			return fmt.Errorf("write trace warning: %w", err)
		}
	}
	return writeResult(cfg, result, stdout)
}

func prepareInput(
	ctx context.Context,
	cfg config,
	stdin io.Reader,
	maxSourceBytes uint32,
) (*sonnetbox.WorkspaceImporter, string, string, error) {
	if cfg.exec {
		importer, err := openWorkspace(cfg.root, cfg.jpaths)
		return importer, "<cmdline>", cfg.input, err
	}
	if cfg.input == "-" {
		source, err := readBounded(ctx, stdin, int64(maxSourceBytes))
		if err != nil {
			return nil, "", "", err
		}
		importer, err := openWorkspace(cfg.root, cfg.jpaths)
		return importer, "<stdin>", string(source), err
	}

	root, filename, err := fileWorkspace(cfg.root, cfg.input)
	if err != nil {
		return nil, "", "", err
	}
	importer, err := openWorkspace(root, cfg.jpaths)
	return importer, filename, "", err
}

func openWorkspace(root string, jpaths []string) (*sonnetbox.WorkspaceImporter, error) {
	if root == "" {
		if len(jpaths) > 0 {
			return nil, errors.New("--jpath requires a file input or --root")
		}
		return nil, nil
	}
	options, err := searchOptions(root, jpaths)
	if err != nil {
		return nil, err
	}
	importer, err := sonnetbox.NewWorkspaceImporter(root, options...)
	if err != nil {
		return nil, err
	}
	return importer, nil
}

// searchOptions maps every -J path to one workspace option in declaration
// order, so library paths and search roots share a single precedence list and
// match go-jsonnet FileImporter search order.
//
// A path inside the workspace root becomes a library path. A path outside it
// becomes a separate search root grant, which is how an absolute or
// parent-directory JPath from a go-jsonnet FileImporter is expressed without
// widening the workspace root itself.
func searchOptions(root string, jpaths []string) ([]sonnetbox.WorkspaceOption, error) {
	options := make([]sonnetbox.WorkspaceOption, 0, len(jpaths))
	taken := make(map[string]struct{}, len(jpaths))
	for _, jpath := range jpaths {
		if virtual, err := pathWithinRoot(root, jpath); err == nil {
			options = append(options, sonnetbox.WithLibraryPaths(virtual))
			continue
		}
		mount, err := mountName(root, jpath, taken)
		if err != nil {
			return nil, fmt.Errorf("JPath %q: %w", jpath, err)
		}
		taken[mount] = struct{}{}
		options = append(options, sonnetbox.WithSearchRoot(mount, jpath))
	}
	return options, nil
}

// mountName derives a stable virtual name for an out-of-root JPath from its
// last path element, numbering repeats in declaration order and skipping any
// name the workspace root already uses.
func mountName(root string, jpath string, taken map[string]struct{}) (string, error) {
	absolute, err := filepath.Abs(jpath)
	if err != nil {
		return "", err
	}
	base := sanitizeMountName(filepath.Base(absolute))
	if base == "" {
		base = "jpath"
	}
	for suffix := 1; suffix <= maxMountAttempts; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		if _, exists := taken[candidate]; exists {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, candidate)); err == nil {
			continue
		}
		return candidate, nil
	}
	return "", errors.New("no free virtual name for the library path")
}

// sanitizeMountName reduces one host path element to a name the importer
// accepts as a single virtual path segment.
func sanitizeMountName(element string) string {
	var builder strings.Builder
	for _, symbol := range element {
		switch {
		case symbol >= 'a' && symbol <= 'z',
			symbol >= 'A' && symbol <= 'Z',
			symbol >= '0' && symbol <= '9',
			symbol == '-', symbol == '_', symbol == '.':
			builder.WriteRune(symbol)
		default:
			builder.WriteRune('_')
		}
	}
	return strings.Trim(builder.String(), ".")
}

func fileWorkspace(configuredRoot, input string) (string, string, error) {
	var root string
	var entry string
	var err error
	if configuredRoot == "" {
		entry, err = filepath.Abs(input)
		if err != nil {
			return "", "", fmt.Errorf("resolve input file: %w", err)
		}
		root = filepath.Dir(entry)
	} else {
		root, err = filepath.Abs(configuredRoot)
		if err != nil {
			return "", "", fmt.Errorf("resolve workspace root: %w", err)
		}
		if filepath.IsAbs(input) {
			entry = filepath.Clean(input)
		} else {
			entry = filepath.Join(root, input)
		}
	}
	filename, err := relativeVirtualPath(root, entry)
	if err != nil {
		return "", "", fmt.Errorf("input file %q: %w", input, err)
	}
	return root, filename, nil
}

func pathWithinRoot(root, name string) (string, error) {
	var candidate string
	if filepath.IsAbs(name) {
		candidate = filepath.Clean(name)
	} else {
		candidate = filepath.Join(root, name)
	}
	return relativeVirtualPath(root, candidate)
}

func relativeVirtualPath(root, candidate string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil {
		return "", err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside the workspace root")
	}
	return filepath.ToSlash(relative), nil
}

func readBounded(ctx context.Context, input io.Reader, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("stdin exceeds source limit of %d bytes", limit)
	}
	return data, nil
}

func outputMode(cfg config) sonnetbox.OutputMode {
	switch {
	case cfg.multiDir != "":
		return sonnetbox.OutputModeMulti
	case cfg.stream:
		return sonnetbox.OutputModeStream
	default:
		return sonnetbox.OutputModeSingle
	}
}

func writeResult(cfg config, result sonnetbox.Result, stdout io.Writer) (returnErr error) {
	if cfg.multiDir != "" {
		return writeMulti(cfg, result.Files, stdout)
	}
	writer, closeWriter, err := outputWriter(cfg.outputFile, cfg.createOutputDirs, stdout)
	if err != nil {
		return err
	}
	if closeWriter != nil {
		defer func() {
			if err := closeWriter(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("close output file: %w", err)
			}
		}()
	}
	if cfg.stream {
		for _, document := range result.Documents {
			if _, err := io.WriteString(writer, "---\n"); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			if _, err := writer.Write(document); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
		}
		if len(result.Documents) > 0 {
			if _, err := io.WriteString(writer, "...\n"); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
		}
		return nil
	}
	if _, err := writer.Write(result.Output); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func outputWriter(
	filename string,
	createDirs bool,
	stdout io.Writer,
) (io.Writer, func() error, error) {
	if filename == "" {
		return stdout, nil, nil
	}
	if createDirs {
		parent := filepath.Dir(filename)
		if err := os.MkdirAll(parent, 0o750); err != nil {
			return nil, nil, fmt.Errorf("create output directory: %w", err)
		}
	}
	// The output path is an explicit operator grant, never a Jsonnet-generated name.
	file, err := os.OpenFile( //nolint:gosec // See the operator-grant comment above.
		filename,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		0o600,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open output file: %w", err)
	}
	return file, file.Close, nil
}

func writeMulti(
	cfg config,
	files map[string][]byte,
	stdout io.Writer,
) (returnErr error) {
	if cfg.createOutputDirs {
		if err := os.MkdirAll(cfg.multiDir, 0o750); err != nil {
			return fmt.Errorf("create multi-file output directory: %w", err)
		}
	}
	root, err := os.OpenRoot(cfg.multiDir)
	if err != nil {
		return fmt.Errorf("open multi-file output directory: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close multi-file output directory: %w", err)
		}
	}()

	manifest, closeManifest, err := outputWriter(cfg.outputFile, cfg.createOutputDirs, stdout)
	if err != nil {
		return err
	}
	if closeManifest != nil {
		defer func() {
			if err := closeManifest(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("close multi-file manifest: %w", err)
			}
		}()
	}

	names := make([]string, 0, len(files))
	for name := range files {
		if err := validateOutputName(name); err != nil {
			return fmt.Errorf("unsafe multi-file output name %q: %w", name, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hostName := filepath.FromSlash(name)
		if _, err := fmt.Fprintln(manifest, filepath.Join(cfg.multiDir, hostName)); err != nil {
			return fmt.Errorf("write multi-file manifest: %w", err)
		}
		content := files[name]
		existing, err := root.ReadFile(hostName)
		if err == nil && bytes.Equal(existing, content) {
			continue
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read existing output %q: %w", name, err)
		}
		if cfg.createOutputDirs {
			if err := root.MkdirAll(filepath.Dir(hostName), 0o755); err != nil {
				return fmt.Errorf("create output parent for %q: %w", name, err)
			}
		}
		if err := root.WriteFile(hostName, content, 0o600); err != nil {
			return fmt.Errorf("write output %q: %w", name, err)
		}
	}
	return nil
}

func validateOutputName(name string) error {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return errors.New("name must be a nonempty relative slash path")
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("name contains an empty or traversing component")
		}
	}
	if path.Clean(name) != name {
		return errors.New("name is not canonical")
	}
	return nil
}

func writeVersion(output io.Writer) {
	version := releaseVersion
	if version == "" {
		version = "(devel)"
		if executable, err := os.Executable(); err == nil {
			if info, err := buildinfo.ReadFile(executable); err == nil && info.Main.Version != "" {
				version = info.Main.Version
			}
		}
	}
	evaluator := sonnetbox.Version()
	_, _ = fmt.Fprintf(
		output,
		"sonnetbox %s (jsonnet %s, ABI %d)\n",
		version,
		evaluator.Jsonnet,
		evaluator.ABI,
	)
}

func writeUsage(output io.Writer) {
	_, _ = fmt.Fprintf(output, `Usage: sonnetbox [options] <filename|-|expression>

Evaluate Jsonnet in a fresh WebAssembly sandbox.

Input and imports:
  -e, --exec                  Treat the positional input as Jsonnet code
      --root <dir>            Grant a read-only import workspace
  -J, --jpath <dir>           Add a library path; one outside the workspace
                              is granted as its own read-only root
      --timeout <duration>    Evaluation deadline (default 5s)

Variables and arguments (name=value is required; no environment inference):
  -V, --ext-str <name=value>  External string
      --ext-code <name=code>  External Jsonnet code
  -A, --tla-str <name=value>  Top-level string argument
      --tla-code <name=code>  Top-level Jsonnet argument
      --ext-str-file <name=file>   Read an external string from a file
      --ext-code-file <name=file>  Read external Jsonnet code from a file
      --tla-str-file <name=file>   Read a top-level string from a file
      --tla-code-file <name=file>  Read top-level Jsonnet code from a file

Output:
  -o, --output-file <file>    Write output instead of stdout
  -m, --multi <dir>           Write a multi-file result beneath dir
  -c, --create-output-dirs    Create output parent directories
  -y, --yaml-stream           Write a stream of JSON documents
  -S, --string                Manifest top-level strings as plain text
      --no-trailing-newline   Omit the normal trailing newline

Sandbox policy (sizes accept suffixes such as 512KiB or 16MB):
      --policy <file>         Load ceilings from a JSON policy file
      --print-policy          Print the effective policy and exit
  -s, --max-stack <frames>    Evaluator stack ceiling
%s

Guest compilation cache (compiled code, keep it private to this user):
      --cache-dir <dir>       Cache location (default: user cache directory)
      --no-cache              Compile the guest on every run

Diagnostics:
      --error-format <format> Report failures as text (default) or json

Other:
  -h, --help                  Show this help
  -v, --version               Show Sonnetbox, Jsonnet, and ABI versions

JSONNET_PATH is ignored. Formatter and linter commands are not provided.

Exit status: %d success, %d usage or host failure, %d Jsonnet error,
%d exhausted budget, %d denied import, %d canceled or timed out.
`, policyUsage(),
		exitSuccess, exitFailure, exitEvaluation,
		exitLimit, exitDenied, exitCanceled)
}
