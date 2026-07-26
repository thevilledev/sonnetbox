//go:build race

package sonnetbox

import (
	"context"
	"os"
	"testing"
)

var raceCompilationCache = NewCompilationCache()

func newEngineForTest(ctx context.Context, config EngineConfig) (*Engine, error) {
	return NewEngine(ctx, config, WithCompilationCache(raceCompilationCache))
}

func TestMain(m *testing.M) {
	code := m.Run()
	_ = raceCompilationCache.Close(context.Background())
	os.Exit(code)
}
