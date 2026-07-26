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

func TestRunDefaultFileWorkspaceConfinesImports(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "secret.libsonnet"), `"secret"`)
	main := filepath.Join(root, "app", "main.jsonnet")
	writeTestFile(t, main, `import "../secret.libsonnet"`)

	status, _, stderr := run(context.Background(), t, []string{main}, "")
	if status != 1 || !strings.Contains(stderr, "denied") {
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
	if status != 1 || !strings.Contains(stderr, "denied") {
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
	if status != 1 || !strings.Contains(stderr, "path escapes") {
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
	if status != 1 || !strings.Contains(stderr, "canceled") {
		t.Fatalf("timed Run() = status %d, stderr %q", status, stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, _, stderr = run(ctx, t, []string{"-e", "1"}, "")
	if status != 1 || !strings.Contains(stderr, "canceled") {
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
