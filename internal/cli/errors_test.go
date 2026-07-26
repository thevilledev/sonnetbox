package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thevilledev/sonnetbox"
)

func TestClassifyErrorMapsSonnetboxErrors(t *testing.T) {
	t.Parallel()

	cause := errors.New("cause")
	tests := []struct {
		name     string
		err      error
		wantKind string
		wantExit int
	}{
		{
			name:     "cancellation",
			err:      &sonnetbox.CancellationError{Err: context.Canceled},
			wantKind: "cancellation",
			wantExit: exitCanceled,
		},
		{
			name:     "limit",
			err:      &sonnetbox.LimitError{Resource: "fuel", Limit: 10, Actual: 11},
			wantKind: "limit",
			wantExit: exitLimit,
		},
		{
			name:     "import denied",
			err:      &sonnetbox.ImportDeniedError{ImportedPath: "a", Err: cause},
			wantKind: "import_denied",
			wantExit: exitDenied,
		},
		{
			name:     "import failure",
			err:      &sonnetbox.ImportError{ImportedPath: "a", Err: cause},
			wantKind: "import",
			wantExit: exitFailure,
		},
		{
			name:     "capability",
			err:      &sonnetbox.CapabilityError{Name: "lookup", Err: cause},
			wantKind: "capability",
			wantExit: exitFailure,
		},
		{
			name:     "evaluation",
			err:      &sonnetbox.EvaluationError{Err: cause},
			wantKind: "evaluation",
			wantExit: exitEvaluation,
		},
		{
			name:     "invalid request",
			err:      &sonnetbox.InvalidRequestError{Field: "Source", Err: cause},
			wantKind: "invalid_request",
			wantExit: exitFailure,
		},
		{
			name:     "abi",
			err:      &sonnetbox.ABIError{Err: cause},
			wantKind: "abi",
			wantExit: exitFailure,
		},
		{
			name:     "guest trap",
			err:      &sonnetbox.GuestTrapError{Operation: "evaluation", Err: cause},
			wantKind: "guest_trap",
			wantExit: exitFailure,
		},
		{
			name:     "engine closed",
			err:      &sonnetbox.EngineClosedError{},
			wantKind: "engine_closed",
			wantExit: exitFailure,
		},
		{
			name:     "unclassified",
			err:      errors.New("something else"),
			wantKind: "error",
			wantExit: exitFailure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report, status := classifyError(test.err)
			if report.Kind != test.wantKind || status != test.wantExit {
				t.Fatalf("classifyError() = %q/%d, want %q/%d",
					report.Kind, status, test.wantKind, test.wantExit)
			}
			if report.Error == "" {
				t.Fatal("classifyError() must always report a message")
			}
		})
	}
}

func TestRunJSONErrorFormatDescribesTheFailure(t *testing.T) {
	t.Parallel()

	status, stdout, stderr := run(context.Background(), t, []string{
		"--error-format", "json", "-e", `error "nope"`,
	}, "")
	if status != exitEvaluation || stdout != "" {
		t.Fatalf("Run() = status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	var report errorReport
	if err := json.Unmarshal([]byte(stderr), &report); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
	}
	if report.Kind != "evaluation" || !strings.Contains(report.Error, "nope") {
		t.Fatalf("report = %#v", report)
	}
}

func TestRunJSONErrorFormatReportsLimitDetail(t *testing.T) {
	t.Parallel()

	status, _, stderr := run(context.Background(), t, []string{
		"--error-format=json", "--max-output-bytes", "8",
		"-e", `{a: "a value that is comfortably over eight bytes"}`,
	}, "")
	if status != exitLimit {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	var report errorReport
	if err := json.Unmarshal([]byte(stderr), &report); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
	}
	if report.Kind != "limit" || report.Limit != 8 || report.Actual <= report.Limit {
		t.Fatalf("report = %#v", report)
	}
	if report.Resource == "" {
		t.Fatal("a limit report must name the exhausted resource")
	}
}

func TestRunJSONErrorFormatReportsDeniedImport(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "secret.libsonnet"), `"secret"`)
	main := filepath.Join(root, "app", "main.jsonnet")
	writeTestFile(t, main, `import "../secret.libsonnet"`)

	status, _, stderr := run(context.Background(), t, []string{"--error-format=json", main}, "")
	if status != exitDenied {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	var report errorReport
	if err := json.Unmarshal([]byte(stderr), &report); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
	}
	if report.Kind != "import_denied" || report.ImportedPath != "../secret.libsonnet" {
		t.Fatalf("report = %#v", report)
	}
}

func TestRunTextErrorFormatRemainsTheDefault(t *testing.T) {
	t.Parallel()

	status, _, stderr := run(context.Background(), t, []string{"-e", `error "nope"`}, "")
	if status != exitEvaluation {
		t.Fatalf("Run() = status %d, stderr %q", status, stderr)
	}
	if !strings.HasPrefix(stderr, "sonnetbox: ") {
		t.Fatalf("stderr = %q, want the human-readable form", stderr)
	}
}

func TestParseArgsRejectsUnknownErrorFormat(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"--error-format", "yaml", "input"},
		{"--error-format", "", "input"},
		{"--error-format"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("parseArgs(%q) error = nil", args)
		}
	}
}

func TestUsageDocumentsExitStatuses(t *testing.T) {
	t.Parallel()

	_, stdout, _ := run(context.Background(), t, []string{"--help"}, "")
	if !strings.Contains(stdout, "Exit status:") {
		t.Fatalf("help output does not document exit statuses:\n%s", stdout)
	}
}
