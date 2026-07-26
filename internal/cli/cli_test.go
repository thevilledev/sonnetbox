package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-jsonnet"
	"github.com/thevilledev/sonnetbox"
)

func TestParseArgs(t *testing.T) {
	t.Parallel()

	cfg, err := parseArgs([]string{
		"-J", "vendor",
		"--jpath=lib",
		"-V", "name=first",
		"--ext-str", "name=last",
		"--ext-code", "count=1 + 1",
		"-A", "target=prod",
		"--tla-code", "replicas=3",
		"--timeout=2s",
		"--",
		"-input.jsonnet",
	})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if cfg.input != "-input.jsonnet" ||
		cfg.timeout != 2*time.Second ||
		cfg.extVars["name"] != "last" ||
		cfg.extCode["count"] != "1 + 1" ||
		cfg.tlaVars["target"] != "prod" ||
		cfg.tlaCode["replicas"] != "3" {
		t.Fatalf("parseArgs() config = %#v", cfg)
	}
	if got := strings.Join(cfg.jpaths, ","); got != "vendor,lib" {
		t.Fatalf("jpaths = %q, want vendor,lib", got)
	}
}

func TestParseArgsRejectsInvalidCombinations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "missing input"},
		{name: "multiple inputs", args: []string{"one", "two"}},
		{name: "bad timeout", args: []string{"--timeout", "0s", "input"}},
		{name: "binding without value", args: []string{"-V", "name", "input"}},
		{name: "stream and multi", args: []string{"-y", "-m", "out", "input"}},
		{name: "stream and newline", args: []string{"-y", "--no-trailing-newline", "input"}},
		{name: "create without output", args: []string{"-c", "input"}},
		{name: "unknown option", args: []string{"--wat", "input"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseArgs(test.args); err == nil {
				t.Fatal("parseArgs() error = nil")
			}
		})
	}
}

func TestRunInlineVariablesAndOutputModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "external values",
			args: []string{
				"-e",
				"-V", "name=sonnetbox",
				"--ext-code", "answer=6 * 7",
				`{name: std.extVar("name"), answer: std.extVar("answer")}`,
			},
			want: "{\n   \"answer\": 42,\n   \"name\": \"sonnetbox\"\n}\n",
		},
		{
			name: "top-level arguments",
			args: []string{
				"-e",
				"-A", "name=service",
				"--tla-code", "replicas=2 + 1",
				`function(name, replicas) {name: name, replicas: replicas}`,
			},
			want: "{\n   \"name\": \"service\",\n   \"replicas\": 3\n}\n",
		},
		{
			name: "string without newline",
			args: []string{
				"-e", "-S", "--no-trailing-newline", `"plain text"`,
			},
			want: "plain text",
		},
		{
			name: "stream",
			args: []string{"-e", "-y", `[1, {two: 2}]`},
			want: "---\n1\n---\n{\n   \"two\": 2\n}\n...\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status, stdout, stderr := run(context.Background(), t, test.args, "")
			if status != 0 || stdout != test.want || stderr != "" {
				t.Fatalf(
					"Run() = status %d, stdout %q, stderr %q; want 0, %q, empty",
					status,
					stdout,
					stderr,
					test.want,
				)
			}
		})
	}
}

func TestRunMatchesGoJsonnet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		source        string
		args          []string
		stringOutput  bool
		outputNewline bool
	}{
		{
			name:          "object",
			source:        `{answer: 6 * 7, evaluator: "jsonnet"}`,
			outputNewline: true,
		},
		{
			name:          "plain string",
			source:        `"plain text"`,
			args:          []string{"-S", "--no-trailing-newline"},
			stringOutput:  true,
			outputNewline: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			vm := jsonnet.MakeVM()
			vm.StringOutput = test.stringOutput
			vm.OutputNewline = test.outputNewline
			want, err := vm.EvaluateAnonymousSnippet("<cmdline>", test.source)
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-e"}, test.args...)
			args = append(args, test.source)
			status, stdout, stderr := run(context.Background(), t, args, "")
			if status != 0 || stderr != "" || stdout != want {
				t.Fatalf(
					"Run() = status %d, stdout %q, stderr %q; go-jsonnet = %q",
					status,
					stdout,
					stderr,
					want,
				)
			}
		})
	}
}

func TestRunFileWorkspaceAndJPathPrecedence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "apps", "main.jsonnet"), `{
  peer: import "peer.libsonnet",
  library: import "choice.libsonnet",
}`)
	writeTestFile(t, filepath.Join(root, "apps", "peer.libsonnet"), `"peer"`)
	writeTestFile(t, filepath.Join(root, "first", "choice.libsonnet"), `"first"`)
	writeTestFile(t, filepath.Join(root, "second", "choice.libsonnet"), `"second"`)

	status, stdout, stderr := run(context.Background(), t, []string{
		"--root", root,
		"-J", "first",
		"-J", "second",
		"apps/main.jsonnet",
	}, "")
	if status != 0 || stderr != "" {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	want := "{\n   \"library\": \"second\",\n   \"peer\": \"peer\"\n}\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunReadsVariableFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeTestFile(t, filepath.Join(directory, "name.txt"), "production")
	writeTestFile(t, filepath.Join(directory, "replicas.jsonnet"), "1 + 2")
	writeTestFile(t, filepath.Join(directory, "region.txt"), "eu-north-1")
	writeTestFile(t, filepath.Join(directory, "flags.jsonnet"), `{beta: true}`)

	status, stdout, stderr := run(context.Background(), t, []string{
		"--ext-str-file", "environment=" + filepath.Join(directory, "name.txt"),
		"--ext-code-file", "replicas=" + filepath.Join(directory, "replicas.jsonnet"),
		"--tla-str-file", "region=" + filepath.Join(directory, "region.txt"),
		"--tla-code-file", "flags=" + filepath.Join(directory, "flags.jsonnet"),
		"-e", `function(region, flags) {
  environment: std.extVar("environment"),
  replicas: std.extVar("replicas"),
  region: region,
  beta: flags.beta,
}`,
	}, "")
	if status != 0 || stderr != "" {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	want := "{\n   \"beta\": true,\n   \"environment\": \"production\"," +
		"\n   \"region\": \"eu-north-1\",\n   \"replicas\": 3\n}\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunRejectsUnusableVariableFiles(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.txt")
	status, _, stderr := run(context.Background(), t, []string{
		"--ext-str-file", "value=" + missing,
		"-e", `std.extVar("value")`,
	}, "")
	if status != exitFailure || !strings.Contains(stderr, "open variable file") {
		t.Fatalf("Run() = status %d, stderr %q; want a missing-file failure", status, stderr)
	}

	oversized := filepath.Join(t.TempDir(), "big.txt")
	writeTestFile(t, oversized, strings.Repeat("x", 64))
	status, _, stderr = run(context.Background(), t, []string{
		"--max-source-bytes", "32",
		"--ext-str-file", "value=" + oversized,
		"-e", `std.extVar("value")`,
	}, "")
	if status != exitFailure || !strings.Contains(stderr, "exceeds source limit") {
		t.Fatalf("Run() = status %d, stderr %q; want a source-limit failure", status, stderr)
	}
}

func TestParseArgsExplainsMigrationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "bare external string",
			args: []string{"-V", "ENVIRONMENT", "input.jsonnet"},
			want: "never infers a value from the environment",
		},
		{
			name: "empty external string",
			args: []string{"-V", "=value", "input.jsonnet"},
			want: "is not in name=value form",
		},
		{
			name: "max trace",
			args: []string{"-t", "20", "input.jsonnet"},
			want: "stack-trace cropping is not supported",
		},
		{
			name: "garbage collector tuning",
			args: []string{"--gc-min-objects", "1000", "input.jsonnet"},
			want: "see --max-memory",
		},
		{
			name: "unknown option",
			args: []string{"--not-a-flag", "input.jsonnet"},
			want: `unrecognized option "--not-a-flag"`,
		},
		{
			name: "variable file without a name",
			args: []string{"--ext-str-file", "values.txt", "input.jsonnet"},
			want: "is not in name=file form",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseArgs(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseArgs() error = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestRunShortVersionFlag(t *testing.T) {
	t.Parallel()

	status, stdout, stderr := run(context.Background(), t, []string{"-v"}, "")
	if status != 0 || stderr != "" || !strings.HasPrefix(stdout, "sonnetbox ") {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
}

func TestRunReportsIgnoredJsonnetPath(t *testing.T) {
	t.Setenv("JSONNET_PATH", string(filepath.ListSeparator)+"lib")

	status, stdout, stderr := run(context.Background(), t, []string{"-e", `1`}, "")
	if status != 0 || stdout != "1\n" {
		t.Fatalf("Run() = status %d, stdout %q", status, stdout)
	}
	if !strings.Contains(stderr, "JSONNET_PATH is ignored") {
		t.Fatalf("stderr = %q, want a JSONNET_PATH notice", stderr)
	}
}

func TestRunGrantsJPathsOutsideTheWorkspaceRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "apps", "main.jsonnet"), `import "choice.libsonnet"`)
	writeTestFile(t, filepath.Join(root, "inside", "choice.libsonnet"), `"inside"`)
	external := filepath.Join(t.TempDir(), "vendor")
	writeTestFile(t, filepath.Join(external, "choice.libsonnet"), `"outside"`)

	// A JPath outside the workspace root is granted its own read-only root
	// instead of being refused, and shares one precedence list with in-root
	// library paths so the last declaration wins, as in go-jsonnet.
	for _, test := range []struct {
		name  string
		paths []string
		want  string
	}{
		{name: "outside last", paths: []string{"inside", external}, want: "\"outside\"\n"},
		{name: "inside last", paths: []string{external, "inside"}, want: "\"inside\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			args := []string{"--root", root}
			for _, jpath := range test.paths {
				args = append(args, "-J", jpath)
			}
			args = append(args, "apps/main.jsonnet")

			vm := jsonnet.MakeVM()
			vm.Importer(&jsonnet.FileImporter{JPaths: hostJPaths(root, test.paths)})
			want, err := vm.EvaluateFile(filepath.Join(root, "apps", "main.jsonnet"))
			if err != nil {
				t.Fatal(err)
			}
			if want != test.want {
				t.Fatalf("go-jsonnet returned %q, want %q", want, test.want)
			}

			status, stdout, stderr := run(context.Background(), t, args, "")
			if status != 0 || stderr != "" || stdout != want {
				t.Fatalf(
					"Run() = status %d, stdout %q, stderr %q; go-jsonnet = %q",
					status, stdout, stderr, want,
				)
			}
		})
	}
}

func TestRunConfinesJPathsGrantedOutsideTheRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "main.jsonnet"), `import "../secret.libsonnet"`)
	external := filepath.Join(t.TempDir(), "lib")
	writeTestFile(t, filepath.Join(external, "present.libsonnet"), `1`)
	writeTestFile(t, filepath.Join(filepath.Dir(external), "secret.libsonnet"), `"secret"`)

	// The grant covers the named directory only; its parent stays unreachable.
	status, _, stderr := run(context.Background(), t, []string{
		"--root", root,
		"-J", external,
		"main.jsonnet",
	}, "")
	if status != exitDenied || !strings.Contains(stderr, "denied") {
		t.Fatalf("Run() = status %d, stderr %q; want denied import", status, stderr)
	}
}

func TestSearchOptionsNamesOutOfRootJPathsDeterministically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "vendor"), 0o750); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	first := filepath.Join(parent, "one", "vendor")
	second := filepath.Join(parent, "two", "vendor")
	for _, directory := range []string{first, second} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	// Repeated basenames are numbered in declaration order, and a name the
	// workspace root already uses is skipped so no grant is shadowed.
	if _, err := searchOptions(root, []string{first, second}); err != nil {
		t.Fatalf("searchOptions() error = %v", err)
	}
	taken := make(map[string]struct{})
	names := make([]string, 0, 2)
	for _, directory := range []string{first, second} {
		name, err := mountName(root, directory, taken)
		if err != nil {
			t.Fatal(err)
		}
		taken[name] = struct{}{}
		names = append(names, name)
	}
	if names[0] != "vendor-2" || names[1] != "vendor-3" {
		t.Fatalf("mount names = %q, want [vendor-2 vendor-3]", names)
	}

	if _, err := searchOptions(root, []string{filepath.Join(parent, "..")}); err != nil {
		t.Fatalf("searchOptions() for an unnameable path error = %v", err)
	}
}

func TestSanitizeMountName(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"vendor":     "vendor",
		"my libs":    "my_libs",
		"a/b":        "a_b",
		"..":         "",
		".":          "",
		".hidden":    "hidden",
		"lib-1_2.v3": "lib-1_2.v3",
	} {
		if got := sanitizeMountName(input); got != want {
			t.Errorf("sanitizeMountName(%q) = %q, want %q", input, got, want)
		}
	}
}

// hostJPaths converts CLI -J values into the host paths go-jsonnet expects, so
// a differential check uses the same search order.
func hostJPaths(root string, paths []string) []string {
	hostPaths := make([]string, 0, len(paths))
	for _, jpath := range paths {
		if filepath.IsAbs(jpath) {
			hostPaths = append(hostPaths, jpath)
			continue
		}
		hostPaths = append(hostPaths, filepath.Join(root, jpath))
	}
	return hostPaths
}

func TestRunDefaultFileWorkspaceConfinesImports(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "secret.libsonnet"), `"secret"`)
	main := filepath.Join(root, "app", "main.jsonnet")
	writeTestFile(t, main, `import "../secret.libsonnet"`)

	status, _, stderr := run(context.Background(), t, []string{main}, "")
	if status != exitDenied || !strings.Contains(stderr, "denied") {
		t.Fatalf("Run() = status %d, stderr %q; want denied import", status, stderr)
	}

	status, stdout, stderr := run(context.Background(), t, []string{
		"--root", root,
		"app/main.jsonnet",
	}, "")
	if status != 0 || stdout != "\"secret\"\n" || stderr != "" {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
}

func TestRunStdinImportsRequireRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "value.libsonnet"), `42`)
	source := `import "value.libsonnet"`

	status, _, stderr := run(context.Background(), t, []string{"-"}, source)
	if status != exitDenied || !strings.Contains(stderr, "denied") {
		t.Fatalf("Run() = status %d, stderr %q; want denied import", status, stderr)
	}

	status, stdout, stderr := run(context.Background(), t, []string{
		"--root", root,
		"-",
	}, source)
	if status != 0 || stdout != "42\n" || stderr != "" {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
}

func TestRunRejectsEscapingInputAndSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "outside.jsonnet"), `1`)

	status, _, stderr := run(context.Background(), t, []string{
		"--root", root,
		filepath.Join(outside, "outside.jsonnet"),
	}, "")
	if status != 1 || !strings.Contains(stderr, "outside the workspace") {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}

	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(
		filepath.Join(outside, "outside.jsonnet"),
		filepath.Join(root, "escape.jsonnet"),
	); err != nil {
		t.Fatal(err)
	}
	status, _, stderr = run(context.Background(), t, []string{
		"--root", root,
		"escape.jsonnet",
	}, "")
	// os.Root refuses to follow the link without an exported sentinel error, so
	// the importer reports it as a failure to serve the path rather than a
	// policy denial. Either way the link is never followed.
	if status != exitFailure || !strings.Contains(stderr, "path escapes") {
		t.Fatalf("Run() = status %d, stderr %q; want escaping symlink failure", status, stderr)
	}
}

func TestRunMultiOutputIsSafeAndPreservesUnchangedFiles(t *testing.T) {
	t.Parallel()

	outputDir := filepath.Join(t.TempDir(), "generated")
	args := []string{
		"-e", "-m", outputDir, "-c",
		`{"nested/a.txt": "alpha", "b.txt": "beta"}`,
	}
	status, stdout, stderr := run(context.Background(), t, args, "")
	if status != 0 || stderr != "" {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	wantManifest := filepath.Join(outputDir, "b.txt") + "\n" +
		filepath.Join(outputDir, "nested", "a.txt") + "\n"
	if stdout != wantManifest {
		t.Fatalf("manifest = %q, want %q", stdout, wantManifest)
	}
	aPath := filepath.Join(outputDir, "nested", "a.txt")
	if got := readTestFile(t, aPath); got != "\"alpha\"\n" {
		t.Fatalf("a.txt = %q", got)
	}
	before, err := os.Stat(aPath)
	if err != nil {
		t.Fatal(err)
	}
	status, _, stderr = run(context.Background(), t, args, "")
	if status != 0 || stderr != "" {
		t.Fatalf("second Run() = status %d, stderr %q", status, stderr)
	}
	after, err := os.Stat(aPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged file mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}

	status, _, stderr = run(context.Background(), t, []string{
		"-e", "-m", outputDir,
		`{"../escape.txt": "bad"}`,
	}, "")
	if status != 1 || !strings.Contains(stderr, "unsafe multi-file output name") {
		t.Fatalf("unsafe Run() = status %d, stderr %q", status, stderr)
	}
}

func TestRunOutputFileTraceLimitsAndCancellation(t *testing.T) {
	t.Parallel()

	output := filepath.Join(t.TempDir(), "nested", "result.json")
	status, stdout, stderr := run(context.Background(), t, []string{
		"-e", "-o", output, "-c",
		`std.trace("rendering", {ok: true})`,
	}, "")
	if status != 0 || stdout != "" || !strings.Contains(stderr, "rendering") {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	if got := readTestFile(t, output); got != "{\n   \"ok\": true\n}\n" {
		t.Fatalf("output file = %q", got)
	}

	oversized := int(sonnetbox.DefaultEngineConfig().MaxSourceBytes) + 1
	status, _, stderr = run(context.Background(), t, []string{"-"}, strings.Repeat("x", oversized))
	if status != 1 || !strings.Contains(stderr, "exceeds source limit") {
		t.Fatalf("oversized Run() = status %d, stderr %q", status, stderr)
	}

	status, _, stderr = run(context.Background(), t, []string{
		"-e", "--timeout", "1ns", `std.range(1, 1000000)`,
	}, "")
	if status != exitCanceled || !strings.Contains(stderr, "canceled") {
		t.Fatalf("timed Run() = status %d, stderr %q", status, stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, _, stderr = run(ctx, t, []string{"-e", "1"}, "")
	if status != exitCanceled || !strings.Contains(stderr, "canceled") {
		t.Fatalf("canceled Run() = status %d, stderr %q", status, stderr)
	}
}

func TestRunHelpVersionAndUsageExitCodes(t *testing.T) {
	t.Parallel()

	status, stdout, stderr := run(context.Background(), t, []string{"--help"}, "")
	if status != 0 || !strings.Contains(stdout, "Usage: sonnetbox") || stderr != "" {
		t.Fatalf("help Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	status, stdout, stderr = run(context.Background(), t, []string{"--version"}, "")
	if status != 0 ||
		!strings.Contains(stdout, "sonnetbox ") ||
		!strings.Contains(stdout, "jsonnet v0.22.0") ||
		!strings.Contains(stdout, "ABI ") ||
		stderr != "" {
		t.Fatalf("version Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	status, stdout, stderr = run(context.Background(), t, nil, "")
	if status != 1 || stdout != "" || !strings.Contains(stderr, "exactly one") {
		t.Fatalf("usage Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
}

func run(
	ctx context.Context,
	t *testing.T,
	args []string,
	stdin string,
) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	status := Run(ctx, args, strings.NewReader(stdin), &stdout, &stderr)
	return status, stdout.String(), stderr.String()
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name) //nolint:gosec // Test paths are created beneath t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
