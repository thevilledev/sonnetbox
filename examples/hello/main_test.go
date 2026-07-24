package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(t.Context(), &output); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Message   string `json:"message"`
		Sandboxed bool   `json:"sandboxed"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Message != "Hello, sonnetbox!" || !got.Sandboxed {
		t.Fatalf("unexpected output: %+v", got)
	}
	if !bytes.HasSuffix(output.Bytes(), []byte("\n")) {
		t.Fatal("output is missing Jsonnet's trailing newline")
	}
}

func TestRunHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var output bytes.Buffer
	if err := run(ctx, &output); err == nil {
		t.Fatal("expected canceled context to fail")
	}
}
