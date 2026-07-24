// Command hello demonstrates one isolated inline Jsonnet evaluation.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"time"

	"github.com/thevilledev/sonnetbox"
)

const helloSource = `{
  message: "Hello, " + std.extVar("name") + "!",
  sandboxed: true,
}`

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, output io.Writer) (runErr error) {
	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, engine.Close(context.WithoutCancel(ctx)))
	}()

	evaluationCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	result, err := engine.Evaluate(evaluationCtx, sonnetbox.Request{
		Filename: "hello.jsonnet",
		Source:   helloSource,
		ExtVars:  map[string]string{"name": "sonnetbox"},
	})
	if err != nil {
		return err
	}
	if _, err := output.Write(result.Output); err != nil {
		return err
	}
	return nil
}
