package sonnetbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
)

// Option customizes an Engine beyond the resource ceilings in EngineConfig.
// Options control how the guest is compiled and executed; they never widen the
// sandbox or raise a resource limit.
type Option func(*engineOptions) error

type engineOptions struct {
	cache       *CompilationCache
	interpreter bool
}

// CompilationCache stores compiled guest code so that engines can skip
// recompiling the embedded module. Compiling the guest dominates engine
// creation, so a shared cache is the difference between seconds and
// milliseconds for short-lived processes.
//
// A cache is safe for concurrent use and may back any number of engines. It is
// owned by the caller: Engine.Close never closes it, so close it only after
// every engine using it has been closed.
type CompilationCache struct {
	inner     wazero.CompilationCache
	closeOnce sync.Once
	closeErr  error
}

// NewCompilationCache returns an in-memory cache shared by the engines that
// use it. It speeds up creating several engines in one process but does not
// survive process exit.
func NewCompilationCache() *CompilationCache {
	return &CompilationCache{inner: wazero.NewCompilationCache()}
}

// NewCompilationCacheDir returns a cache persisted beneath dir, reused across
// processes. It is the useful form for short-lived commands, which otherwise
// recompile the guest on every invocation.
//
// A cache directory holds executable machine code that later runs in this
// process. Point it at a directory only the current user can write, such as a
// path beneath os.UserCacheDir, and never at a world-writable or
// shared-tenant location.
func NewCompilationCacheDir(dir string) (*CompilationCache, error) {
	if dir == "" {
		return nil, &InvalidRequestError{
			Field: "compilation cache directory",
			Err:   errors.New("path is empty"),
		}
	}
	inner, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("open compilation cache %q: %w", dir, err)
	}
	return &CompilationCache{inner: inner}, nil
}

// Close releases the cache. It is idempotent, and callers must close every
// engine that used the cache first.
func (c *CompilationCache) Close(ctx context.Context) error {
	if c == nil || c.inner == nil {
		return nil
	}
	if ctx == nil {
		return &InvalidRequestError{Field: "context", Err: errors.New("context is nil")}
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.inner.Close(ctx)
	})
	return c.closeErr
}

// WithCompilationCache reuses compiled guest code from cache. Engines sharing
// a cache still get isolated runtimes and fresh guest instances per
// evaluation; only the compiled code is shared.
func WithCompilationCache(cache *CompilationCache) Option {
	return func(options *engineOptions) error {
		if cache == nil || cache.inner == nil {
			return &InvalidRequestError{
				Field: "compilation cache",
				Err:   errors.New("cache is nil"),
			}
		}
		options.cache = cache
		return nil
	}
}

// WithInterpreter runs the guest on wazero's portable interpreter instead of
// the optimizing compiler. It starts far faster but evaluates far slower, so
// it suits one-shot evaluations and platforms without compiler support.
func WithInterpreter() Option {
	return func(options *engineOptions) error {
		options.interpreter = true
		return nil
	}
}

func newEngineOptions(options []Option) (engineOptions, error) {
	resolved := engineOptions{}
	for _, option := range options {
		if option == nil {
			return engineOptions{}, &InvalidRequestError{
				Field: "engine option",
				Err:   errors.New("option is nil"),
			}
		}
		if err := option(&resolved); err != nil {
			return engineOptions{}, err
		}
	}
	return resolved, nil
}

func (o engineOptions) runtimeConfig() wazero.RuntimeConfig {
	var config wazero.RuntimeConfig
	if o.interpreter {
		config = wazero.NewRuntimeConfigInterpreter()
	} else {
		config = wazero.NewRuntimeConfig()
	}
	if o.cache != nil {
		config = config.WithCompilationCache(o.cache.inner)
	}
	return config
}
