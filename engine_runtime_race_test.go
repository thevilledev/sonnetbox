//go:build race

package securejsonnet

import (
	"context"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"
)

var raceCompilationCache = wazero.NewCompilationCache()

func newEngineForTest(ctx context.Context, config EngineConfig) (*Engine, error) {
	runtimeConfig := wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(raceCompilationCache)
	return newEngine(ctx, config, runtimeConfig)
}

func TestMain(m *testing.M) {
	code := m.Run()
	_ = raceCompilationCache.Close(context.Background())
	os.Exit(code)
}
