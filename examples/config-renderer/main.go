// Command config-renderer renders a file from a read-only Jsonnet workspace.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/thevilledev/wasmnet"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

func run(
	ctx context.Context,
	args []string,
	output io.Writer,
	errorOutput io.Writer,
) (runErr error) {
	flags := flag.NewFlagSet("config-renderer", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	workspacePath := flags.String(
		"workspace",
		"",
		"path to the Jsonnet workspace",
	)
	environment := flags.String(
		"environment",
		"development",
		"configuration environment (development or production)",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *workspacePath == "" {
		return errors.New("-workspace is required")
	}
	if *environment != "development" && *environment != "production" {
		return fmt.Errorf(
			"-environment must be development or production, got %q",
			*environment,
		)
	}

	workspace, err := wasmnet.NewWorkspaceImporter(
		*workspacePath,
		wasmnet.WithLibraryPaths("lib"),
	)
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, workspace.Close())
	}()

	engine, err := wasmnet.NewEngine(ctx, wasmnet.EngineConfig{})
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, engine.Close(context.WithoutCancel(ctx)))
	}()

	evaluationCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	result, err := engine.EvaluateFile(
		evaluationCtx,
		"apps/main.jsonnet",
		wasmnet.Request{
			Importer: workspace,
			ExtVars:  map[string]string{"environment": *environment},
		},
	)
	if err != nil {
		return err
	}
	if _, err := output.Write(result.Output); err != nil {
		return err
	}
	return nil
}
