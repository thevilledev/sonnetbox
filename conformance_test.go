package sonnetbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

type conformanceSuite struct {
	Cases []conformanceCase `json:"cases"`
}

type conformanceCase struct {
	Name         string                           `json:"name"`
	InputMode    string                           `json:"input_mode"`
	Request      conformanceRequest               `json:"request"`
	Imports      map[string]string                `json:"imports"`
	Capabilities map[string]conformanceCapability `json:"capabilities"`
	Expected     conformanceExpected              `json:"expected"`
}

type conformanceRequest struct {
	Filename     string            `json:"filename"`
	Source       string            `json:"source"`
	ExtVars      map[string]string `json:"ext_vars"`
	OutputMode   string            `json:"output_mode"`
	CaptureTrace bool              `json:"capture_trace"`
}

type conformanceCapability struct {
	Params []string        `json:"params"`
	Args   []any           `json:"args"`
	Result json.RawMessage `json:"result"`
}

type conformanceExpected struct {
	Output        json.RawMessage            `json:"output"`
	Files         map[string]json.RawMessage `json:"files"`
	Documents     []json.RawMessage          `json:"documents"`
	Imports       []string                   `json:"imports"`
	TraceContains string                     `json:"trace_contains"`
}

func TestPortableHostConformance(t *testing.T) {
	raw, err := os.ReadFile("abi/v7/conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var suite conformanceSuite
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&suite); err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) == 0 {
		t.Fatal("conformance suite has no cases")
	}

	engine := newTestEngine(t, EngineConfig{})
	for _, testCase := range suite.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			request := Request{
				Filename:     testCase.Request.Filename,
				Source:       testCase.Request.Source,
				ExtVars:      testCase.Request.ExtVars,
				CaptureTrace: testCase.Request.CaptureTrace,
			}
			switch testCase.Request.OutputMode {
			case "", "single":
				request.OutputMode = OutputModeSingle
			case "multi":
				request.OutputMode = OutputModeMulti
			case "stream":
				request.OutputMode = OutputModeStream
			default:
				t.Fatalf("unknown output mode %q", testCase.Request.OutputMode)
			}
			if len(testCase.Imports) != 0 {
				files := make(map[string][]byte, len(testCase.Imports))
				for name, content := range testCase.Imports {
					files[name] = []byte(content)
				}
				request.Importer, err = NewMapImporter(files)
				if err != nil {
					t.Fatal(err)
				}
			}
			request.Capabilities = make(map[string]Capability, len(testCase.Capabilities))
			for name, declaration := range testCase.Capabilities {
				var value any
				if err := json.Unmarshal(declaration.Result, &value); err != nil {
					t.Fatalf("decode capability %q result: %v", name, err)
				}
				request.Capabilities[name] = Capability{
					Params: declaration.Params,
					Call: func(_ context.Context, args []any) (any, error) {
						if !reflect.DeepEqual(args, declaration.Args) {
							return nil, fmt.Errorf(
								"arguments %#v do not match fixture %#v",
								args,
								declaration.Args,
							)
						}
						return value, nil
					},
				}
			}

			var result Result
			switch testCase.InputMode {
			case "snippet":
				result, err = engine.Evaluate(context.Background(), request)
			case "anonymous":
				result, err = engine.EvaluateAnonymous(context.Background(), request)
			case "file":
				result, err = engine.EvaluateFile(
					context.Background(),
					request.Filename,
					request,
				)
			default:
				t.Fatalf("unknown input mode %q", testCase.InputMode)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertConformanceResult(t, result, testCase.Expected)
		})
	}
}

func assertConformanceResult(t *testing.T, result Result, expected conformanceExpected) {
	t.Helper()
	if expected.Output != nil {
		var value any
		if err := json.Unmarshal(expected.Output, &value); err != nil {
			t.Fatal(err)
		}
		assertJSON(t, result.Output, value)
	}
	if expected.Files != nil {
		if len(result.Files) != len(expected.Files) {
			t.Fatalf("files = %d, want %d", len(result.Files), len(expected.Files))
		}
		for name, encoded := range expected.Files {
			var value any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, result.Files[name], value)
		}
	}
	if expected.Documents != nil {
		if len(result.Documents) != len(expected.Documents) {
			t.Fatalf(
				"documents = %d, want %d",
				len(result.Documents),
				len(expected.Documents),
			)
		}
		for index, encoded := range expected.Documents {
			var value any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, result.Documents[index], value)
		}
	}
	if !reflect.DeepEqual(result.Imports, expected.Imports) {
		t.Fatalf("imports = %#v, want %#v", result.Imports, expected.Imports)
	}
	if expected.TraceContains != "" &&
		!bytes.Contains(result.Trace, []byte(expected.TraceContains)) {
		t.Fatalf("trace %q does not contain %q", result.Trace, expected.TraceContains)
	}
	if result.Stats.FuelConsumed == 0 {
		t.Fatal("conformance evaluation consumed no fuel")
	}
	if result.Stats.FuelModel != fuelModel {
		t.Fatalf("fuel model = %q, want %q", result.Stats.FuelModel, fuelModel)
	}
}
