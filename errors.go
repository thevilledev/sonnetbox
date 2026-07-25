package sonnetbox

import "fmt"

// InvalidRequestError reports an invalid public request, context, or engine
// configuration.
type InvalidRequestError struct {
	// Field identifies the rejected public field when one is available.
	Field string
	// Err describes why the request, context, or configuration was invalid.
	Err error
}

func (e *InvalidRequestError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("invalid request: %v", e.Err)
	}
	return fmt.Sprintf("invalid request field %q: %v", e.Field, e.Err)
}
func (e *InvalidRequestError) Unwrap() error { return e.Err }

// ImportDeniedError reports an import rejected by policy or not found.
type ImportDeniedError struct {
	// ImportedFrom is the canonical path of the importing file, or empty when
	// resolving from the importer root.
	ImportedFrom string
	// ImportedPath is the path requested by Jsonnet.
	ImportedPath string
	// Err describes the policy denial or missing path.
	Err error
}

func (e *ImportDeniedError) Error() string {
	return fmt.Sprintf("import %q from %q denied: %v", e.ImportedPath, e.ImportedFrom, e.Err)
}
func (e *ImportDeniedError) Unwrap() error { return e.Err }

// ImportError reports a trusted importer failure.
type ImportError struct {
	// ImportedFrom is the canonical path of the importing file, or empty when
	// resolving from the importer root.
	ImportedFrom string
	// ImportedPath is the path requested by Jsonnet.
	ImportedPath string
	// Err is the underlying trusted importer failure.
	Err error
}

func (e *ImportError) Error() string {
	return fmt.Sprintf("import %q from %q failed: %v", e.ImportedPath, e.ImportedFrom, e.Err)
}
func (e *ImportError) Unwrap() error { return e.Err }

// CapabilityError reports a trusted capability failure.
type CapabilityError struct {
	// Name identifies the failed capability when one is available.
	Name string
	// Err is the underlying trusted capability failure.
	Err error
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("capability %q failed: %v", e.Name, e.Err)
}
func (e *CapabilityError) Unwrap() error { return e.Err }

// LimitError reports a configured resource limit.
type LimitError struct {
	// Resource identifies the exhausted resource.
	Resource string
	// Limit is the configured maximum.
	Limit uint64
	// Actual is the observed or attempted resource use.
	Actual uint64
	// Err is the underlying runtime error, when one is available.
	Err error
}

func (e *LimitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s limit exceeded (%d > %d): %v", e.Resource, e.Actual, e.Limit, e.Err)
	}
	return fmt.Sprintf("%s limit exceeded (%d > %d)", e.Resource, e.Actual, e.Limit)
}
func (e *LimitError) Unwrap() error { return e.Err }

// CancellationError reports evaluation cancellation or deadline expiry.
type CancellationError struct {
	// Err is the context cancellation cause.
	Err error
}

func (e *CancellationError) Error() string { return fmt.Sprintf("evaluation canceled: %v", e.Err) }
func (e *CancellationError) Unwrap() error { return e.Err }

// GuestTrapError reports an unexpected WASM trap.
type GuestTrapError struct {
	// Operation identifies the guest operation that trapped.
	Operation string
	// Err is the underlying WebAssembly runtime error.
	Err error
}

func (e *GuestTrapError) Error() string {
	return fmt.Sprintf("guest trapped during %s: %v", e.Operation, e.Err)
}
func (e *GuestTrapError) Unwrap() error { return e.Err }

// EvaluationError reports a static or runtime Jsonnet evaluation error.
type EvaluationError struct {
	// Err contains the evaluator's diagnostic.
	Err error
}

func (e *EvaluationError) Error() string { return fmt.Sprintf("jsonnet evaluation failed: %v", e.Err) }
func (e *EvaluationError) Unwrap() error { return e.Err }

// ABIError reports a malformed or incompatible guest/host ABI.
type ABIError struct {
	// Err describes the protocol or guest artifact failure.
	Err error
}

func (e *ABIError) Error() string { return fmt.Sprintf("guest ABI error: %v", e.Err) }
func (e *ABIError) Unwrap() error { return e.Err }

// EngineClosedError reports an evaluation attempted after Engine.Close.
type EngineClosedError struct {
	// Err is the underlying runtime error when an active evaluation was
	// interrupted by Close.
	Err error
}

func (e *EngineClosedError) Error() string {
	if e.Err == nil {
		return "sonnetbox engine is closed"
	}
	return fmt.Sprintf("sonnetbox engine is closed: %v", e.Err)
}
func (e *EngineClosedError) Unwrap() error { return e.Err }
