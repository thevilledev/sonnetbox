//go:build !race

package sonnetbox

import "context"

func newEngineForTest(ctx context.Context, config EngineConfig) (*Engine, error) {
	return NewEngine(ctx, config)
}
