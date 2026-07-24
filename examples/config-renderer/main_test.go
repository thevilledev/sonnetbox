package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunProduction(t *testing.T) {
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	err := run(
		t.Context(),
		[]string{"-workspace", "jsonnet", "-environment", "production"},
		&output,
		&errorOutput,
	)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Environment string `json:"environment"`
		Service     struct {
			LogLevel string `json:"logLevel"`
			Name     string `json:"name"`
			Replicas int    `json:"replicas"`
		} `json:"service"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Environment != "production" ||
		got.Service.Name != "catalog" ||
		got.Service.Replicas != 3 ||
		got.Service.LogLevel != "info" {
		t.Fatalf("unexpected output: %+v", got)
	}
}

func TestRunValidatesFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "workspace is required",
			args: nil,
			want: "-workspace is required",
		},
		{
			name: "environment is constrained",
			args: []string{"-workspace", "jsonnet", "-environment", "staging"},
			want: "-environment must be development or production",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			var errorOutput bytes.Buffer
			err := run(t.Context(), test.args, &output, &errorOutput)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
