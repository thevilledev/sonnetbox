package sonnetbox

import (
	"errors"
	"testing"
)

func TestErrorTypesFormatAndUnwrap(t *testing.T) {
	cause := errors.New("cause")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "invalid request",
			err:  &InvalidRequestError{Err: cause},
			want: "invalid request: cause",
		},
		{
			name: "invalid request field",
			err:  &InvalidRequestError{Field: "source", Err: cause},
			want: `invalid request field "source": cause`,
		},
		{
			name: "import denied",
			err: &ImportDeniedError{
				ImportedFrom: "main.jsonnet",
				ImportedPath: "secret.jsonnet",
				Err:          cause,
			},
			want: `import "secret.jsonnet" from "main.jsonnet" denied: cause`,
		},
		{
			name: "import failure",
			err: &ImportError{
				ImportedFrom: "main.jsonnet",
				ImportedPath: "value.jsonnet",
				Err:          cause,
			},
			want: `import "value.jsonnet" from "main.jsonnet" failed: cause`,
		},
		{
			name: "capability failure",
			err:  &CapabilityError{Name: "lookup", Err: cause},
			want: `capability "lookup" failed: cause`,
		},
		{
			name: "limit with cause",
			err: &LimitError{
				Resource: "output",
				Limit:    10,
				Actual:   11,
				Err:      cause,
			},
			want: "output limit exceeded (11 > 10): cause",
		},
		{
			name: "cancellation",
			err:  &CancellationError{Err: cause},
			want: "evaluation canceled: cause",
		},
		{
			name: "guest trap",
			err:  &GuestTrapError{Operation: "evaluate", Err: cause},
			want: "guest trapped during evaluate: cause",
		},
		{
			name: "evaluation failure",
			err:  &EvaluationError{Err: cause},
			want: "jsonnet evaluation failed: cause",
		},
		{
			name: "ABI failure",
			err:  &ABIError{Err: cause},
			want: "guest ABI error: cause",
		},
		{
			name: "closed engine with cause",
			err:  &EngineClosedError{Err: cause},
			want: "sonnetbox engine is closed: cause",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Fatalf("Error() = %q, want %q", got, test.want)
			}
			if !errors.Is(test.err, cause) {
				t.Fatalf("errors.Is(%T, cause) = false", test.err)
			}
		})
	}
}

func TestErrorTypesWithoutCauses(t *testing.T) {
	limit := &LimitError{Resource: "memory", Limit: 64, Actual: 65}
	if got, want := limit.Error(), "memory limit exceeded (65 > 64)"; got != want {
		t.Fatalf("LimitError.Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(limit) != nil {
		t.Fatal("LimitError.Unwrap() returned a non-nil error")
	}

	closed := &EngineClosedError{}
	if got, want := closed.Error(), "sonnetbox engine is closed"; got != want {
		t.Fatalf("EngineClosedError.Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(closed) != nil {
		t.Fatal("EngineClosedError.Unwrap() returned a non-nil error")
	}
}
