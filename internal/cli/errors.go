package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/thevilledev/sonnetbox"
)

// Exit statuses let a caller distinguish a rejected program from an exhausted
// budget without parsing the message.
const (
	exitSuccess = 0
	// exitFailure covers usage mistakes and host-side failures.
	exitFailure = 1
	// exitEvaluation reports a static or runtime Jsonnet error.
	exitEvaluation = 2
	// exitLimit reports an exhausted sandbox budget.
	exitLimit = 3
	// exitDenied reports an import refused by sandbox policy.
	exitDenied = 4
	// exitCanceled reports a deadline or cancellation.
	exitCanceled = 5
)

const (
	errorFormatText = "text"
	errorFormatJSON = "json"
)

// errorReport is the machine-readable form of a failed run.
type errorReport struct {
	Error        string `json:"error"`
	Kind         string `json:"kind"`
	Resource     string `json:"resource,omitempty"`
	Limit        uint64 `json:"limit,omitempty"`
	Actual       uint64 `json:"actual,omitempty"`
	ImportedPath string `json:"imported_path,omitempty"`
	ImportedFrom string `json:"imported_from,omitempty"`
}

// reportError writes err in the requested format and returns the exit status.
func reportError(stderr io.Writer, format string, err error) int {
	report, status := classifyError(err)
	if format == errorFormatJSON {
		encoder := json.NewEncoder(stderr)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(report); encodeErr != nil {
			_, _ = fmt.Fprintf(stderr, "sonnetbox: %v\n", err)
		}
		return status
	}
	_, _ = fmt.Fprintf(stderr, "sonnetbox: %v\n", err)
	return status
}

// classifyError maps a sonnetbox error onto a stable kind and exit status.
// The order matters: the most specific classification must win.
func classifyError(err error) (errorReport, int) {
	report := errorReport{Error: err.Error(), Kind: "error"}

	var canceled *sonnetbox.CancellationError
	if errors.As(err, &canceled) {
		report.Kind = "cancellation"
		return report, exitCanceled
	}
	var limit *sonnetbox.LimitError
	if errors.As(err, &limit) {
		report.Kind = "limit"
		report.Resource = limit.Resource
		report.Limit = limit.Limit
		report.Actual = limit.Actual
		return report, exitLimit
	}
	var denied *sonnetbox.ImportDeniedError
	if errors.As(err, &denied) {
		report.Kind = "import_denied"
		report.ImportedPath = denied.ImportedPath
		report.ImportedFrom = denied.ImportedFrom
		return report, exitDenied
	}
	var importErr *sonnetbox.ImportError
	if errors.As(err, &importErr) {
		report.Kind = "import"
		report.ImportedPath = importErr.ImportedPath
		report.ImportedFrom = importErr.ImportedFrom
		return report, exitFailure
	}
	var capability *sonnetbox.CapabilityError
	if errors.As(err, &capability) {
		report.Kind = "capability"
		return report, exitFailure
	}
	var evaluation *sonnetbox.EvaluationError
	if errors.As(err, &evaluation) {
		report.Kind = "evaluation"
		return report, exitEvaluation
	}
	var invalid *sonnetbox.InvalidRequestError
	if errors.As(err, &invalid) {
		report.Kind = "invalid_request"
		return report, exitFailure
	}
	var abi *sonnetbox.ABIError
	if errors.As(err, &abi) {
		report.Kind = "abi"
		return report, exitFailure
	}
	var trap *sonnetbox.GuestTrapError
	if errors.As(err, &trap) {
		report.Kind = "guest_trap"
		return report, exitFailure
	}
	var closed *sonnetbox.EngineClosedError
	if errors.As(err, &closed) {
		report.Kind = "engine_closed"
		return report, exitFailure
	}
	return report, exitFailure
}
