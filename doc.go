// Package sonnetbox evaluates untrusted Jsonnet programs in fresh WebAssembly
// guests.
//
// An [Engine] compiles the embedded go-jsonnet guest once and creates an
// isolated guest instance for each evaluation. Engines are safe for concurrent
// use and should normally be long-lived:
//
//	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
//	if err != nil {
//		return err
//	}
//	defer engine.Close(context.Background())
//
//	result, err := engine.Evaluate(ctx, sonnetbox.Request{
//		Filename: "main.jsonnet",
//		Source:   `{answer: 6 * 7}`,
//	})
//
// Jsonnet code has no ambient access to the host filesystem, network,
// environment, arguments, or standard streams. A request can grant read-only
// virtual imports through an [Importer] and pure native functions through
// [Capability]. Both are trusted host code; implementations must honor context
// cancellation and the concurrency contracts documented on those types.
//
// [EngineConfig] sets engine-wide resource ceilings. A [Request] can lower
// most ceilings for one evaluation through [RequestLimits], but cannot raise
// them. Context cancellation provides the wall-clock backstop.
package sonnetbox
