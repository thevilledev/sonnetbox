package sonnetbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Observer receives notifications about sandbox activity, so a host can build
// an audit trail or metrics without inferring behavior from return values. A
// nil field is skipped, and adding a field later does not break existing
// implementations.
//
// Hooks run inline on the evaluation path, inside the host call the guest is
// blocked on. They must return promptly and must not panic: a panic during an
// import or capability call is reported to the guest as a handler failure, and
// one during the evaluation hook unwinds to the caller.
//
// Events describe activity, never content. They carry sizes, counts, and
// paths, but no imported bytes and no capability arguments, so an audit log
// cannot become an accidental copy of the data flowing through the sandbox.
type Observer struct {
	// Import runs after every import attempt, whether it was served, refused
	// by policy, or failed.
	Import func(ctx context.Context, event ImportEvent)
	// Capability runs after every native function call.
	Capability func(ctx context.Context, event CapabilityEvent)
	// Evaluation runs once per evaluation, after it succeeds or fails.
	Evaluation func(ctx context.Context, event EvaluationEvent)
}

// ImportEvent describes one import attempt.
type ImportEvent struct {
	// ImportedFrom is the canonical path of the importing file, empty for the
	// top-level source.
	ImportedFrom string
	// ImportedPath is the path exactly as the Jsonnet program requested it.
	ImportedPath string
	// ResolvedPath is the canonical path the importer returned, empty unless
	// the import was served.
	ResolvedPath string
	// Bytes is the size of the served content, zero unless the import was
	// served.
	Bytes int
	// Duration covers validation, the importer call, and limit accounting.
	Duration time.Duration
	// Denied reports that sandbox policy refused the import. This is the
	// security-relevant outcome, distinct from an importer that failed.
	Denied bool
	// Err is the reason the import did not succeed, nil when it did.
	Err error
}

// CapabilityEvent describes one native function call.
type CapabilityEvent struct {
	// Name is the capability as declared in the request.
	Name string
	// Args is the number of arguments passed, not their values.
	Args int
	// Duration covers decoding, the handler call, and encoding the reply.
	Duration time.Duration
	// Err is the reason the call did not succeed, nil when it did.
	Err error
}

// EvaluationEvent describes one completed evaluation.
type EvaluationEvent struct {
	// Filename is the evaluated file or snippet name.
	Filename string
	// Stats is the same value the evaluation reported, including for a
	// failure that reached the point of producing statistics.
	Stats EvaluationStats
	// Err is the reason the evaluation failed, nil when it succeeded.
	Err error
}

// WithObserver reports sandbox activity to observer for audit and diagnostics.
// An observer cannot change any outcome; it only watches.
func WithObserver(observer *Observer) Option {
	return func(options *engineOptions) error {
		if observer == nil {
			return &InvalidRequestError{
				Field: "observer",
				Err:   errors.New("observer is nil"),
			}
		}
		copied := *observer
		options.observer = &copied
		return nil
	}
}

// NewSlogObserver returns an Observer that writes sandbox activity to logger.
// Denied imports are logged at warn level because they are the
// security-relevant events; failures are logged at error level and ordinary
// activity at debug level.
func NewSlogObserver(logger *slog.Logger) *Observer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Observer{
		Import: func(ctx context.Context, event ImportEvent) {
			attributes := []any{
				slog.String("imported_path", event.ImportedPath),
				slog.String("imported_from", event.ImportedFrom),
				slog.Duration("duration", event.Duration),
			}
			switch {
			case event.Denied:
				logger.WarnContext(ctx, "sonnetbox import denied",
					append(attributes, slog.Any("error", event.Err))...)
			case event.Err != nil:
				logger.ErrorContext(ctx, "sonnetbox import failed",
					append(attributes, slog.Any("error", event.Err))...)
			default:
				logger.DebugContext(ctx, "sonnetbox import resolved",
					append(attributes,
						slog.String("resolved_path", event.ResolvedPath),
						slog.Int("bytes", event.Bytes),
					)...)
			}
		},
		Capability: func(ctx context.Context, event CapabilityEvent) {
			attributes := []any{
				slog.String("capability", event.Name),
				slog.Int("args", event.Args),
				slog.Duration("duration", event.Duration),
			}
			if event.Err != nil {
				logger.ErrorContext(ctx, "sonnetbox capability failed",
					append(attributes, slog.Any("error", event.Err))...)
				return
			}
			logger.DebugContext(ctx, "sonnetbox capability called", attributes...)
		},
		Evaluation: func(ctx context.Context, event EvaluationEvent) {
			attributes := []any{
				slog.String("filename", event.Filename),
				slog.Duration("queue_duration", event.Stats.QueueDuration),
				slog.Duration("execution_duration", event.Stats.ExecutionDuration),
				slog.Uint64("fuel_consumed", event.Stats.FuelConsumed),
				slog.Uint64("import_resolutions", uint64(event.Stats.ImportResolutions)),
				slog.Uint64("import_bytes", event.Stats.ImportBytes),
				slog.Uint64("capability_calls", uint64(event.Stats.CapabilityCalls)),
			}
			if event.Err != nil {
				logger.ErrorContext(ctx, "sonnetbox evaluation failed",
					append(attributes, slog.Any("error", event.Err))...)
				return
			}
			logger.DebugContext(ctx, "sonnetbox evaluation succeeded", attributes...)
		},
	}
}

// observeImport reports an import attempt when an Import hook is installed.
func (e *Engine) observeImport(ctx context.Context, event ImportEvent) {
	if e.observer == nil || e.observer.Import == nil {
		return
	}
	e.observer.Import(ctx, event)
}

// observeCapability reports a native call when a Capability hook is installed.
func (e *Engine) observeCapability(ctx context.Context, event CapabilityEvent) {
	if e.observer == nil || e.observer.Capability == nil {
		return
	}
	e.observer.Capability(ctx, event)
}

// observeEvaluation reports a finished evaluation when a hook is installed.
func (e *Engine) observeEvaluation(ctx context.Context, event EvaluationEvent) {
	if e.observer == nil || e.observer.Evaluation == nil {
		return
	}
	e.observer.Evaluation(ctx, event)
}
