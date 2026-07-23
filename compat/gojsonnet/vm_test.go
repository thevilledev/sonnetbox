package gojsonnet

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	nativejsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	securejsonnet "github.com/thevilledev/wasmnet"
)

func TestDifferentialCommonVMOperations(t *testing.T) {
	engine, err := securejsonnet.NewEngine(
		context.Background(),
		RecommendedEngineConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	t.Run("anonymous variables arguments and native function", func(t *testing.T) {
		nativeVM := nativejsonnet.MakeVM()
		secureVM := newTestVM(t, engine)
		nativeVM.SetTraceOut(nil)
		secureVM.SetTraceOut(nil)

		nativeFunction := &nativejsonnet.NativeFunction{
			Name:   "double",
			Params: ast.Identifiers{"value"},
			Func: func(args []any) (any, error) {
				return args[0].(float64) * 2, nil
			},
		}
		nativeVM.NativeFunction(nativeFunction)
		secureVM.NativeFunction(nativeFunction)
		nativeVM.ExtVar("environment", "prod")
		secureVM.ExtVar("environment", "prod")
		nativeVM.ExtCode("config", `{enabled: true}`)
		secureVM.ExtCode("config", `{enabled: true}`)
		nativeVM.TLACode("value", "21")
		secureVM.TLACode("value", "21")

		source := `function(value) {
			environment: std.extVar("environment"),
			config: std.extVar("config"),
			answer: std.native("double")(value),
		}`
		want, err := nativeVM.EvaluateAnonymousSnippet("input.jsonnet", source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"input.jsonnet",
			source,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("secure output differs from go-jsonnet\nwant: %s\ngot: %s", want, got)
		}
	})

	t.Run("multi and stream", func(t *testing.T) {
		nativeVM := nativejsonnet.MakeVM()
		secureVM := newTestVM(t, engine)
		nativeVM.SetTraceOut(nil)
		secureVM.SetTraceOut(nil)

		multiSource := `{["a.json"]: {value: 1}, ["b.json"]: {value: 2}}`
		wantMulti, err := nativeVM.EvaluateAnonymousSnippetMulti(
			"multi.jsonnet",
			multiSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		gotMulti, err := secureVM.EvaluateAnonymousSnippetMulti(
			context.Background(),
			"multi.jsonnet",
			multiSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotMulti, wantMulti) {
			t.Fatalf("multi output differs\nwant: %#v\ngot: %#v", wantMulti, gotMulti)
		}

		streamSource := `[{value: 1}, {value: 2}]`
		wantStream, err := nativeVM.EvaluateAnonymousSnippetStream(
			"stream.jsonnet",
			streamSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		gotStream, err := secureVM.EvaluateAnonymousSnippetStream(
			context.Background(),
			"stream.jsonnet",
			streamSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotStream, wantStream) {
			t.Fatalf("stream output differs\nwant: %#v\ngot: %#v", wantStream, gotStream)
		}
	})

	t.Run("file and importer adapter", func(t *testing.T) {
		memory := &nativejsonnet.MemoryImporter{Data: map[string]nativejsonnet.Contents{
			"main.jsonnet":  nativejsonnet.MakeContents(`import "value.jsonnet"`),
			"value.jsonnet": nativejsonnet.MakeContents(`{answer: 42}`),
		}}
		adapter, err := AdaptImporter(memory)
		if err != nil {
			t.Fatal(err)
		}
		nativeVM := nativejsonnet.MakeVM()
		nativeVM.Importer(memory)
		nativeVM.SetTraceOut(nil)
		secureVM := newTestVM(t, engine)
		secureVM.Importer(adapter)
		secureVM.SetTraceOut(nil)

		want, err := nativeVM.EvaluateFile("main.jsonnet")
		if err != nil {
			t.Fatal(err)
		}
		got, err := secureVM.EvaluateFile(context.Background(), "main.jsonnet")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("file output differs\nwant: %s\ngot: %s", want, got)
		}
	})

	t.Run("trace", func(t *testing.T) {
		nativeVM := nativejsonnet.MakeVM()
		secureVM := newTestVM(t, engine)
		var nativeTrace bytes.Buffer
		var secureTrace bytes.Buffer
		nativeVM.SetTraceOut(&nativeTrace)
		secureVM.SetTraceOut(&secureTrace)
		source := `std.trace("hello", 42)`

		want, err := nativeVM.EvaluateAnonymousSnippet("trace.jsonnet", source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"trace.jsonnet",
			source,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got != want || secureTrace.String() != nativeTrace.String() {
			t.Fatalf(
				"trace evaluation differs\nwant output: %q\n got output: %q\nwant trace: %q\n got trace: %q",
				want,
				got,
				nativeTrace.String(),
				secureTrace.String(),
			)
		}
	})
}

func TestAdaptImporterRejectsFileImporter(t *testing.T) {
	if _, err := AdaptImporter(&nativejsonnet.FileImporter{}); err == nil {
		t.Fatal("expected FileImporter adaptation to be rejected")
	}
}

func newTestVM(t *testing.T, engine *securejsonnet.Engine) *VM {
	t.Helper()
	vm, err := New(engine)
	if err != nil {
		t.Fatal(err)
	}
	return vm
}
