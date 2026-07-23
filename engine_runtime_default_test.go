//go:build !race

package securejsonnet

import "context"

func newEngineForTest(ctx context.Context, config EngineConfig) (*Engine, error) {
	return NewEngine(ctx, config)
}
