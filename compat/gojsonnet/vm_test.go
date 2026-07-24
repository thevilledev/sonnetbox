package gojsonnet

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	nativejsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/thevilledev/sonnetbox"
)

func TestDifferentialCommonVMOperations(t *testing.T) {
	engine, err := sonnetbox.NewEngine(
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

	t.Run("string output and newline", func(t *testing.T) {
		nativeVM := nativejsonnet.MakeVM()
		secureVM := newTestVM(t, engine)
		nativeVM.SetTraceOut(nil)
		secureVM.SetTraceOut(nil)
		nativeVM.StringOutput = true
		secureVM.StringOutput = true
		nativeVM.OutputNewline = false
		secureVM.OutputNewline = false

		const source = `"hello"`
		want, err := nativeVM.EvaluateAnonymousSnippet("string.jsonnet", source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"string.jsonnet",
			source,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("string output differs\nwant: %q\ngot: %q", want, got)
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

	t.Run("named snippets and file output modes", func(t *testing.T) {
		memory := &nativejsonnet.MemoryImporter{Data: map[string]nativejsonnet.Contents{
			"multi.jsonnet": nativejsonnet.MakeContents(
				`{["a.json"]: {value: 1}, ["b.json"]: {value: 2}}`,
			),
			"stream.jsonnet": nativejsonnet.MakeContents(`[{value: 1}, {value: 2}]`),
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

		const singleSource = `{answer: 6 * 7}`
		wantSingle, err := nativeVM.EvaluateAnonymousSnippet("snippet.jsonnet", singleSource)
		if err != nil {
			t.Fatal(err)
		}
		gotSingle, err := secureVM.EvaluateSnippet(
			context.Background(),
			"snippet.jsonnet",
			singleSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		if gotSingle != wantSingle {
			t.Fatalf("snippet output differs\nwant: %s\ngot: %s", wantSingle, gotSingle)
		}

		const multiSource = `{["inline.json"]: {value: 3}}`
		wantMulti, err := nativeVM.EvaluateAnonymousSnippetMulti("snippet.jsonnet", multiSource)
		if err != nil {
			t.Fatal(err)
		}
		gotMulti, err := secureVM.EvaluateSnippetMulti(
			context.Background(),
			"snippet.jsonnet",
			multiSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotMulti, wantMulti) {
			t.Fatalf("snippet multi output differs\nwant: %#v\ngot: %#v", wantMulti, gotMulti)
		}

		const streamSource = `[{value: 3}]`
		wantStream, err := nativeVM.EvaluateAnonymousSnippetStream("snippet.jsonnet", streamSource)
		if err != nil {
			t.Fatal(err)
		}
		gotStream, err := secureVM.EvaluateSnippetStream(
			context.Background(),
			"snippet.jsonnet",
			streamSource,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotStream, wantStream) {
			t.Fatalf(
				"snippet stream output differs\nwant: %#v\ngot: %#v",
				wantStream,
				gotStream,
			)
		}

		wantFileMulti, err := nativeVM.EvaluateFileMulti("multi.jsonnet")
		if err != nil {
			t.Fatal(err)
		}
		gotFileMulti, err := secureVM.EvaluateFileMulti(
			context.Background(),
			"multi.jsonnet",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotFileMulti, wantFileMulti) {
			t.Fatalf(
				"file multi output differs\nwant: %#v\ngot: %#v",
				wantFileMulti,
				gotFileMulti,
			)
		}

		wantFileStream, err := nativeVM.EvaluateFileStream("stream.jsonnet")
		if err != nil {
			t.Fatal(err)
		}
		gotFileStream, err := secureVM.EvaluateFileStream(
			context.Background(),
			"stream.jsonnet",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotFileStream, wantFileStream) {
			t.Fatalf(
				"file stream output differs\nwant: %#v\ngot: %#v",
				wantFileStream,
				gotFileStream,
			)
		}
	})

	t.Run("variable and argument resets", func(t *testing.T) {
		secureVM := newTestVM(t, engine)
		secureVM.SetTraceOut(nil)
		secureVM.ExtVar("plain", "external")
		secureVM.ExtCode("code", `{enabled: true}`)
		secureVM.TLAVar("plain", "argument")
		secureVM.TLACode("code", "40 + 2")

		const source = `function(plain, code) {
			extVar: std.extVar("plain"),
			extCode: std.extVar("code"),
			tlaVar: plain,
			tlaCode: code,
		}`
		if _, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"variables.jsonnet",
			source,
		); err != nil {
			t.Fatal(err)
		}

		secureVM.ExtReset()
		secureVM.TLAReset()
		if _, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"variables.jsonnet",
			source,
		); err == nil {
			t.Fatal("expected reset variables and arguments to be unavailable")
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

	t.Run("trace writer failure", func(t *testing.T) {
		secureVM := newTestVM(t, engine)
		writeErr := errors.New("trace sink failed")
		secureVM.SetTraceOut(errorWriter{err: writeErr})
		if _, err := secureVM.EvaluateAnonymousSnippet(
			context.Background(),
			"trace.jsonnet",
			`std.trace("hello", 42)`,
		); !errors.Is(err, writeErr) {
			t.Fatalf("expected trace writer error, got %v", err)
		}
	})
}

func TestAdaptImporterRejectsFileImporter(t *testing.T) {
	if _, err := AdaptImporter(&nativejsonnet.FileImporter{}); err == nil {
		t.Fatal("expected FileImporter adaptation to be rejected")
	}
}

func TestCompatibilityValidation(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("expected a nil engine to be rejected")
	}
	if _, err := AdaptImporter(nil); err == nil {
		t.Fatal("expected a nil importer to be rejected")
	}

	var adapter *ImporterAdapter
	if _, _, err := adapter.Import(
		context.Background(),
		"",
		"value.jsonnet",
	); err == nil {
		t.Fatal("expected an uninitialized importer adapter to fail")
	}

	memory := &nativejsonnet.MemoryImporter{Data: map[string]nativejsonnet.Contents{
		"value.jsonnet": nativejsonnet.MakeContents(`42`),
	}}
	adapter, err := AdaptImporter(memory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := adapter.Import(ctx, "", "value.jsonnet"); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func newTestVM(t *testing.T, engine *sonnetbox.Engine) *VM {
	t.Helper()
	vm, err := New(engine)
	if err != nil {
		t.Fatal(err)
	}
	return vm
}
