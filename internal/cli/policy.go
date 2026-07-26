package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thevilledev/sonnetbox"
)

const (
	cacheDirectoryName = "sonnetbox"
	cacheSubdirectory  = "guest"
	cacheDirectoryMode = 0o700
)

// policyOverride applies one operator-supplied ceiling.
type policyOverride func(*sonnetbox.EngineConfig)

type policyFlag struct {
	placeholder string
	usage       string
	parse       func(string) (policyOverride, error)
}

// policyFlags maps each ceiling to a flag. Sonnetbox owns the defaults and the
// hard maximums, so a flag only ever narrows what the library already allows.
var policyFlags = map[string]policyFlag{
	"--max-memory": {
		placeholder: "<size>",
		usage:       "Guest linear memory ceiling",
		parse: byteSize64(func(policy *sonnetbox.EngineConfig, value uint64) {
			policy.MaxMemoryBytes = value
		}),
	},
	"--max-wasm-stack": {
		placeholder: "<size>",
		usage:       "Compiled WebAssembly call-stack ceiling",
		parse: byteSize64(func(policy *sonnetbox.EngineConfig, value uint64) {
			policy.MaxWasmStackBytes = value
		}),
	},
	"--max-fuel": {
		placeholder: "<units>",
		usage:       "Deterministic instruction budget",
		parse: count64(func(policy *sonnetbox.EngineConfig, value uint64) {
			policy.MaxFuel = value
		}),
	},
	"--max-source-bytes": {
		placeholder: "<size>",
		usage:       "Source ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxSourceBytes = value
		}),
	},
	"--max-output-bytes": {
		placeholder: "<size>",
		usage:       "Rendered output ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxOutputBytes = value
		}),
	},
	"--max-imports": {
		placeholder: "<count>",
		usage:       "Import resolution ceiling",
		parse: count32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxImports = value
		}),
	},
	"--max-import-bytes": {
		placeholder: "<size>",
		usage:       "Single import ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxImportBytes = value
		}),
	},
	"--max-total-import-bytes": {
		placeholder: "<size>",
		usage:       "Cumulative import ceiling",
		parse: byteSize64(func(policy *sonnetbox.EngineConfig, value uint64) {
			policy.MaxTotalImportBytes = value
		}),
	},
	"--max-capability-calls": {
		placeholder: "<count>",
		usage:       "Native capability call ceiling",
		parse: count32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxCapabilityCalls = value
		}),
	},
	"--max-host-request-bytes": {
		placeholder: "<size>",
		usage:       "Guest-to-host message ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxHostRequestBytes = value
		}),
	},
	"--max-host-response-bytes": {
		placeholder: "<size>",
		usage:       "Host-to-guest message ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxHostResponseBytes = value
		}),
	},
	"--max-trace-bytes": {
		placeholder: "<size>",
		usage:       "Captured std.trace ceiling",
		parse: byteSize32(func(policy *sonnetbox.EngineConfig, value uint32) {
			policy.MaxTraceBytes = value
		}),
	},
}

// applyPolicyFlag records a ceiling override, reporting whether name was a
// policy flag at all so the caller can still reject unknown options.
func applyPolicyFlag(
	cfg *config,
	name string,
	next func() (string, error),
) (bool, error) {
	flag, ok := policyFlags[name]
	if !ok {
		return false, nil
	}
	value, err := next()
	if err != nil {
		return false, err
	}
	override, err := flag.parse(value)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	cfg.policyOverrides = append(cfg.policyOverrides, override)
	return true, nil
}

// resolvePolicy layers the library defaults, then an optional policy file,
// then explicit flags, and validates the result.
func resolvePolicy(cfg config) (sonnetbox.EngineConfig, error) {
	policy := sonnetbox.DefaultEngineConfig()
	if cfg.policyFile != "" {
		if err := loadPolicyFile(cfg.policyFile, &policy); err != nil {
			return sonnetbox.EngineConfig{}, err
		}
	}
	for _, override := range cfg.policyOverrides {
		override(&policy)
	}
	effective, err := policy.Normalize()
	if err != nil {
		return sonnetbox.EngineConfig{}, fmt.Errorf("invalid policy: %w", err)
	}
	return effective, nil
}

// loadPolicyFile decodes a JSON policy over the supplied defaults, so an
// absent field keeps its default rather than collapsing to zero.
func loadPolicyFile(name string, policy *sonnetbox.EngineConfig) error {
	// The policy path is an explicit operator grant, never a Jsonnet-derived name.
	data, err := os.ReadFile(name) //nolint:gosec // See the operator-grant comment above.
	if err != nil {
		return fmt.Errorf("read policy file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(policy); err != nil {
		return fmt.Errorf("decode policy file %q: %w", name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("policy file %q must contain exactly one JSON object", name)
	}
	return nil
}

func writePolicy(output io.Writer, cfg config) error {
	policy, err := resolvePolicy(cfg)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(policy); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	return nil
}

// openCompilationCache returns a guest-code cache, or nil when caching is
// disabled or unavailable. Compiling the guest dominates startup, so the cache
// is on by default. A failure to use the implicit location is not fatal: the
// command still runs, just without the saving. An explicitly requested
// directory is an operator instruction, so failing to honor it is an error.
func openCompilationCache(cfg config) (*sonnetbox.CompilationCache, error) {
	if cfg.noCache {
		return nil, nil //nolint:nilnil // a disabled cache is an expected, non-error outcome.
	}
	directory := cfg.cacheDir
	explicit := directory != ""
	if !explicit {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, degradeToNoCache(explicit, err)
		}
		directory = filepath.Join(base, cacheDirectoryName, cacheSubdirectory)
	}
	// The cache holds executable machine code, so keep it private to this user.
	if err := os.MkdirAll(directory, cacheDirectoryMode); err != nil {
		return nil, degradeToNoCache(explicit, fmt.Errorf("create compilation cache directory: %w", err))
	}
	cache, err := sonnetbox.NewCompilationCacheDir(directory)
	if err != nil {
		return nil, degradeToNoCache(explicit, err)
	}
	return cache, nil
}

// degradeToNoCache reports a caching failure only when the operator named the
// directory. An implicit location is an optimization, never a requirement.
func degradeToNoCache(explicit bool, err error) error {
	if explicit {
		return err
	}
	return nil
}

func byteSize64(
	assign func(*sonnetbox.EngineConfig, uint64),
) func(string) (policyOverride, error) {
	return func(value string) (policyOverride, error) {
		size, err := parseByteSize(value)
		if err != nil {
			return nil, err
		}
		return func(policy *sonnetbox.EngineConfig) { assign(policy, size) }, nil
	}
}

func byteSize32(
	assign func(*sonnetbox.EngineConfig, uint32),
) func(string) (policyOverride, error) {
	return func(value string) (policyOverride, error) {
		size, err := parseByteSize(value)
		if err != nil {
			return nil, err
		}
		if size > math.MaxUint32 {
			return nil, fmt.Errorf("%q exceeds the %d byte maximum", value, uint64(math.MaxUint32))
		}
		narrowed := uint32(size)
		return func(policy *sonnetbox.EngineConfig) { assign(policy, narrowed) }, nil
	}
}

func count64(
	assign func(*sonnetbox.EngineConfig, uint64),
) func(string) (policyOverride, error) {
	return func(value string) (policyOverride, error) {
		parsed, err := parseCount(value)
		if err != nil {
			return nil, err
		}
		return func(policy *sonnetbox.EngineConfig) { assign(policy, parsed) }, nil
	}
}

func count32(
	assign func(*sonnetbox.EngineConfig, uint32),
) func(string) (policyOverride, error) {
	return func(value string) (policyOverride, error) {
		parsed, err := parseCount(value)
		if err != nil {
			return nil, err
		}
		if parsed > math.MaxUint32 {
			return nil, fmt.Errorf("%q exceeds the %d maximum", value, uint64(math.MaxUint32))
		}
		narrowed := uint32(parsed)
		return func(policy *sonnetbox.EngineConfig) { assign(policy, narrowed) }, nil
	}
}

func parseCount(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a positive whole number", value)
	}
	if parsed == 0 {
		return 0, errors.New("must be greater than zero")
	}
	return parsed, nil
}

// byteSuffixes must stay ordered longest-first so that "KiB" is matched before
// the bare "B" that ends it.
var byteSuffixes = []struct {
	suffix string
	scale  uint64
}{
	{suffix: "KIB", scale: 1 << 10},
	{suffix: "MIB", scale: 1 << 20},
	{suffix: "GIB", scale: 1 << 30},
	{suffix: "KB", scale: 1000},
	{suffix: "MB", scale: 1000 * 1000},
	{suffix: "GB", scale: 1000 * 1000 * 1000},
	{suffix: "B", scale: 1},
}

// parseByteSize accepts a plain byte count or a binary or decimal suffix, so
// an operator can write 16MiB instead of counting zeros.
func parseByteSize(value string) (uint64, error) {
	digits := strings.TrimSpace(value)
	if digits == "" {
		return 0, errors.New("value is empty")
	}
	scale := uint64(1)
	upper := strings.ToUpper(digits)
	for _, candidate := range byteSuffixes {
		if strings.HasSuffix(upper, candidate.suffix) {
			scale = candidate.scale
			digits = strings.TrimSpace(digits[:len(digits)-len(candidate.suffix)])
			break
		}
	}
	parsed, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte size", value)
	}
	if parsed == 0 {
		return 0, errors.New("must be greater than zero")
	}
	if parsed > math.MaxUint64/scale {
		return 0, fmt.Errorf("%q overflows a byte count", value)
	}
	return parsed * scale, nil
}

// policyUsage renders the policy flags for the help text, ordered so the help
// output stays stable.
func policyUsage() string {
	names := make([]string, 0, len(policyFlags))
	for name := range policyFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		flag := policyFlags[name]
		left := fmt.Sprintf("      %s %s", name, flag.placeholder)
		if len(left) >= usageColumn {
			// Keep the description readable when the flag outgrows the column.
			builder.WriteString(left + "\n")
			builder.WriteString(strings.Repeat(" ", usageColumn) + flag.usage + "\n")
			continue
		}
		_, _ = fmt.Fprintf(&builder, "%-*s%s\n", usageColumn, left, flag.usage)
	}
	return strings.TrimRight(builder.String(), "\n")
}
