package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thevilledev/sonnetbox"
)

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  uint64
	}{
		{name: "plain", input: "1024", want: 1024},
		{name: "bytes", input: "1024B", want: 1024},
		{name: "binary kilo", input: "2KiB", want: 2048},
		{name: "binary mega", input: "16MiB", want: 16 << 20},
		{name: "binary giga", input: "1GiB", want: 1 << 30},
		{name: "decimal kilo", input: "2KB", want: 2000},
		{name: "decimal mega", input: "3MB", want: 3_000_000},
		{name: "lowercase", input: "8mib", want: 8 << 20},
		{name: "spaced", input: " 4KiB ", want: 4096},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseByteSize(test.input)
			if err != nil {
				t.Fatalf("parseByteSize(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("parseByteSize(%q) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}

func TestParseByteSizeRejectsBadValues(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "abc", "-1", "1.5MiB", "0", "0KiB", "MiB", "18446744073709551615KiB"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := parseByteSize(input); err == nil {
				t.Fatalf("parseByteSize(%q) error = nil", input)
			}
		})
	}
}

func TestPolicyFlagsNarrowTheDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseArgs([]string{
		"--max-fuel", "1000000",
		"--max-memory", "16MiB",
		"--max-wasm-stack", "128MiB",
		"--max-imports", "3",
		"--max-output-bytes", "4KiB",
		"--max-trace-bytes=1KiB",
		"-s", "64",
		"input.jsonnet",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := resolvePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxFuel != 1_000_000 ||
		policy.MaxMemoryBytes != 16<<20 ||
		policy.MaxWasmStackBytes != 128<<20 ||
		policy.MaxImports != 3 ||
		policy.MaxOutputBytes != 4<<10 ||
		policy.MaxTraceBytes != 1<<10 ||
		policy.MaxStack != 64 {
		t.Fatalf("resolved policy = %#v", policy)
	}
	// Untouched ceilings must still come from the library defaults.
	if policy.MaxCapabilityCalls != sonnetbox.DefaultEngineConfig().MaxCapabilityCalls {
		t.Fatalf("MaxCapabilityCalls = %d, want the library default", policy.MaxCapabilityCalls)
	}
}

func TestPolicyFlagsCannotExceedLibraryCeilings(t *testing.T) {
	t.Parallel()

	ceilings := sonnetbox.Ceilings()
	cfg, err := parseArgs([]string{
		"--max-fuel", strconv.FormatUint(ceilings.MaxFuel+1, 10),
		"input.jsonnet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePolicy(cfg); err == nil {
		t.Fatal("resolvePolicy() error = nil for a policy above the library ceiling")
	}
}

func TestPolicyFileLayersUnderFlags(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "policy.json")
	writeTestFile(t, path, `{"max_imports": 5, "max_fuel": 2000000}`)

	cfg, err := parseArgs([]string{"--policy", path, "input.jsonnet"})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := resolvePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxImports != 5 || policy.MaxFuel != 2_000_000 {
		t.Fatalf("policy from file = %#v", policy)
	}
	if policy.MaxOutputBytes != sonnetbox.DefaultEngineConfig().MaxOutputBytes {
		t.Fatal("absent policy fields must keep their library defaults")
	}

	// An explicit flag must win over the same field in the file.
	cfg, err = parseArgs([]string{"--policy", path, "--max-imports", "2", "input.jsonnet"})
	if err != nil {
		t.Fatal(err)
	}
	policy, err = resolvePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxImports != 2 {
		t.Fatalf("MaxImports = %d, want the flag value 2", policy.MaxImports)
	}
}

func TestPolicyFileRejectsBadInput(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	unknownField := filepath.Join(directory, "unknown.json")
	writeTestFile(t, unknownField, `{"max_imports": 5, "max_widgets": 2}`)
	trailing := filepath.Join(directory, "trailing.json")
	writeTestFile(t, trailing, `{"max_imports": 5}{"max_imports": 6}`)
	malformed := filepath.Join(directory, "malformed.json")
	writeTestFile(t, malformed, `{`)

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "unknown field", path: unknownField},
		{name: "trailing object", path: trailing},
		{name: "malformed", path: malformed},
		{name: "missing", path: filepath.Join(directory, "absent.json")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := parseArgs([]string{"--policy", test.path, "input.jsonnet"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolvePolicy(cfg); err == nil {
				t.Fatal("resolvePolicy() error = nil")
			}
		})
	}
}

func TestRunPrintPolicyEmitsLoadableJSON(t *testing.T) {
	t.Parallel()

	status, stdout, stderr := run(context.Background(), t, []string{"--print-policy", "--max-imports", "3"}, "")
	if status != 0 || stderr != "" {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	var policy sonnetbox.EngineConfig
	if err := json.Unmarshal([]byte(stdout), &policy); err != nil {
		t.Fatalf("printed policy is not loadable: %v\n%s", err, stdout)
	}
	if policy.MaxImports != 3 {
		t.Fatalf("MaxImports = %d, want 3", policy.MaxImports)
	}
	if policy.MaxFuel != sonnetbox.DefaultEngineConfig().MaxFuel {
		t.Fatal("printed policy must report every effective ceiling")
	}

	// The printed policy must be accepted verbatim as a policy file.
	path := filepath.Join(t.TempDir(), "printed.json")
	writeTestFile(t, path, stdout)
	status, _, stderr = run(context.Background(), t, []string{"--policy", path, "-e", "1"}, "")
	if status != 0 || stderr != "" {
		t.Fatalf("round-tripped policy Run() = status %d, stderr %q", status, stderr)
	}
}

func TestRunRejectsPolicyAboveCeiling(t *testing.T) {
	t.Parallel()

	status, _, stderr := run(context.Background(), t, []string{
		"--max-memory", "8GiB", "-e", "1",
	}, "")
	if status != 1 || !strings.Contains(stderr, "invalid policy") {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
}

func TestRunHonorsALoweredCeiling(t *testing.T) {
	t.Parallel()

	status, _, stderr := run(context.Background(), t, []string{
		"--max-output-bytes", "8", "-e", `{a: "a long enough value to exceed eight bytes"}`,
	}, "")
	if status != exitLimit || !strings.Contains(stderr, "limit exceeded") {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
}

func TestCompilationCacheDirectoryIsUsedAndPrivate(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "cache")
	status, stdout, stderr := run(context.Background(), t, []string{
		"--cache-dir", directory, "-e", "{a: 1}",
	}, "")
	if status != 0 || stderr != "" {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("cache directory was not created: %v", err)
	}
	if mode := info.Mode().Perm(); mode != cacheDirectoryMode {
		t.Fatalf("cache directory mode = %o, want %o", mode, cacheDirectoryMode)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("cache directory is empty, so compiled code was not retained")
	}

	// The second run must reuse the cache and produce identical output.
	status, warm, stderr := run(context.Background(), t, []string{
		"--cache-dir", directory, "-e", "{a: 1}",
	}, "")
	if status != 0 || stderr != "" || warm != stdout {
		t.Fatalf("warm Run() = status %d, stdout %q, stderr %q", status, warm, stderr)
	}
}

func TestNoCacheStillEvaluates(t *testing.T) {
	t.Parallel()

	status, stdout, stderr := run(context.Background(), t, []string{"--no-cache", "-e", "-S", `"hello"`}, "")
	if status != 0 || stderr != "" || stdout != "hello\n" {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
}

func TestParseArgsRejectsBadPolicyArguments(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "cache dir with no cache", args: []string{"--cache-dir", "x", "--no-cache", "input"}},
		{name: "empty cache dir", args: []string{"--cache-dir", "", "input"}},
		{name: "empty policy", args: []string{"--policy", "", "input"}},
		{name: "bad byte size", args: []string{"--max-memory", "lots", "input"}},
		{name: "zero count", args: []string{"--max-imports", "0", "input"}},
		{name: "negative count", args: []string{"--max-fuel", "-5", "input"}},
		{name: "missing value", args: []string{"--max-fuel"}},
		{name: "oversized 32-bit field", args: []string{"--max-trace-bytes", "8GiB", "input"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseArgs(test.args); err == nil {
				t.Fatalf("parseArgs(%q) error = nil", test.args)
			}
		})
	}
}

func TestUsageListsEveryPolicyFlag(t *testing.T) {
	t.Parallel()

	_, stdout, _ := run(context.Background(), t, []string{"--help"}, "")
	for name := range policyFlags {
		if !strings.Contains(stdout, name) {
			t.Fatalf("help output is missing %q", name)
		}
	}
}
