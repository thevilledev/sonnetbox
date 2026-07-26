package sonnetbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMapImporterValidationAndCopy(t *testing.T) {
	original := []byte(`{value: 1}`)
	importer, err := NewMapImporter(map[string][]byte{"lib/value.jsonnet": original})
	if err != nil {
		t.Fatal(err)
	}
	original[0] = 'x'
	canonical, content, err := importer.Import(context.Background(), "lib/main.jsonnet", "value.jsonnet")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "lib/value.jsonnet" || string(content) != `{value: 1}` {
		t.Fatalf("unexpected import: %q %q", canonical, content)
	}
	content[0] = 'x'
	_, content, err = importer.Import(context.Background(), "lib/main.jsonnet", "value.jsonnet")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != `{value: 1}` {
		t.Fatalf("importer returned mutable content: %q", content)
	}

	canonical, _, err = importer.Import(
		context.Background(),
		"lib/nested/main.jsonnet",
		"../value.jsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "lib/value.jsonnet" {
		t.Fatalf("relative import resolved to %q", canonical)
	}

	for _, invalid := range []string{"", "/etc/passwd", "../secret", "a/../b", "./a", "a//b", `a\b`} {
		if _, err := NewMapImporter(map[string][]byte{invalid: nil}); err == nil {
			t.Errorf("expected %q to be rejected", invalid)
		}
	}

	_, _, err = importer.Import(context.Background(), "", "missing.jsonnet")
	if !errors.Is(err, ErrImportDenied) {
		t.Fatalf("expected import denial, got %v", err)
	}
}

func TestWorkspaceImporterRelativeAndLibraryResolution(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "apps/value.jsonnet", `{source: "relative"}`)
	writeTestFile(t, root, "vendor/lib.jsonnet", `{source: "vendor"}`)
	writeTestFile(t, root, "overrides/lib.jsonnet", `{source: "override"}`)

	importer, err := NewWorkspaceImporter(
		root,
		WithLibraryPaths("vendor", "overrides"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := importer.Close(); err != nil {
			t.Error(err)
		}
	})

	canonical, content, err := importer.Import(
		context.Background(),
		"apps/main.jsonnet",
		"./value.jsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "apps/value.jsonnet" || string(content) != `{source: "relative"}` {
		t.Fatalf("unexpected relative import: %q %q", canonical, content)
	}

	canonical, content, err = importer.Import(
		context.Background(),
		"apps/main.jsonnet",
		"lib.jsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "overrides/lib.jsonnet" || string(content) != `{source: "override"}` {
		t.Fatalf("unexpected library import: %q %q", canonical, content)
	}

	if _, _, err := importer.Import(
		context.Background(),
		"apps/main.jsonnet",
		"../../outside.jsonnet",
	); !errors.Is(err, ErrImportDenied) {
		t.Fatalf("expected traversal denial, got %v", err)
	}
}

func TestWithLibraryPathsValidatesBeforeAppending(t *testing.T) {
	existing := []declaredSearchPath{{path: "existing"}}
	config := workspaceConfig{searchPaths: slices.Clone(existing)}
	if err := WithLibraryPaths("vendor", "overrides")(&config); err != nil {
		t.Fatal(err)
	}
	want := []declaredSearchPath{
		{path: "existing"},
		{path: "vendor"},
		{path: "overrides"},
	}
	if !slices.Equal(config.searchPaths, want) {
		t.Fatalf("search paths = %v, want %v", config.searchPaths, want)
	}

	config = workspaceConfig{searchPaths: slices.Clone(existing)}
	if err := WithLibraryPaths("vendor", "../outside")(&config); err == nil {
		t.Fatal("expected invalid library path to be rejected")
	}
	if !slices.Equal(config.searchPaths, existing) {
		t.Fatalf("failed option mutated search paths: %v", config.searchPaths)
	}
}

func TestWithSearchRootValidatesDeclarations(t *testing.T) {
	config := workspaceConfig{}
	if err := WithSearchRoot("stdlib", "/some/where")(&config); err != nil {
		t.Fatal(err)
	}
	want := []declaredSearchPath{{mount: "stdlib", path: "/some/where"}}
	if !slices.Equal(config.searchPaths, want) {
		t.Fatalf("search paths = %v, want %v", config.searchPaths, want)
	}

	if err := WithSearchRoot("stdlib", "/elsewhere")(&config); err == nil {
		t.Fatal("expected a duplicate search root name to be rejected")
	}
	if err := WithSearchRoot("stdlib", "")(&config); err == nil {
		t.Fatal("expected an empty search root path to be rejected")
	}
	for _, invalid := range []string{"", "nested/name", "..", ".", "/abs", `back\slash`} {
		if err := WithSearchRoot(invalid, "/some/where")(&config); err == nil {
			t.Errorf("expected search root name %q to be rejected", invalid)
		}
	}
	if !slices.Equal(config.searchPaths, want) {
		t.Fatalf("failed option mutated search paths: %v", config.searchPaths)
	}
}

func TestWorkspaceImporterSearchRootPrecedence(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "apps/main.jsonnet", `{}`)
	writeTestFile(t, root, "lib/shared.libsonnet", `{source: "in-root lib"}`)

	first := t.TempDir()
	writeTestFile(t, first, "shared.libsonnet", `{source: "first"}`)
	writeTestFile(t, first, "only-first.libsonnet", `{source: "only first"}`)

	second := t.TempDir()
	writeTestFile(t, second, "shared.libsonnet", `{source: "second"}`)

	importer, err := NewWorkspaceImporter(
		root,
		WithLibraryPaths("lib"),
		WithSearchRoot("first", first),
		WithSearchRoot("second", second),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := importer.Close(); err != nil {
			t.Error(err)
		}
	})

	// Every declaration shares one precedence list, so the last one wins.
	canonical, content, err := importer.Import(
		context.Background(),
		"apps/main.jsonnet",
		"shared.libsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "second/shared.libsonnet" || string(content) != `{source: "second"}` {
		t.Fatalf("unexpected precedence winner: %q %q", canonical, content)
	}

	canonical, content, err = importer.Import(
		context.Background(),
		"apps/main.jsonnet",
		"only-first.libsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "first/only-first.libsonnet" || string(content) != `{source: "only first"}` {
		t.Fatalf("unexpected fallback import: %q %q", canonical, content)
	}
}

func TestWorkspaceImporterResolvesRelativeImportsInsideSearchRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "peer.libsonnet", `{source: "workspace root"}`)

	external := t.TempDir()
	writeTestFile(t, external, "pkg/entry.libsonnet", `{}`)
	writeTestFile(t, external, "pkg/peer.libsonnet", `{source: "search root"}`)

	importer, err := NewWorkspaceImporter(root, WithSearchRoot("ext", external))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := importer.Close(); err != nil {
			t.Error(err)
		}
	})

	// A file found in a search root must resolve its own relative imports
	// inside that root rather than falling back to the workspace root.
	canonical, content, err := importer.Import(
		context.Background(),
		"ext/pkg/entry.libsonnet",
		"./peer.libsonnet",
	)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "ext/pkg/peer.libsonnet" || string(content) != `{source: "search root"}` {
		t.Fatalf("unexpected relative import: %q %q", canonical, content)
	}

	if _, _, err := importer.Import(
		context.Background(),
		"ext/pkg/entry.libsonnet",
		"../../escape.libsonnet",
	); !errors.Is(err, ErrImportDenied) {
		t.Fatalf("search root traversal error = %v, want import denial", err)
	}
}

func TestWorkspaceImporterSearchRootRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.jsonnet")
	if err := os.WriteFile(outside, []byte(`"secret"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(external, "escape.jsonnet")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	importer, err := NewWorkspaceImporter(root, WithSearchRoot("ext", external))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := importer.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, content, err := importer.Import(
		context.Background(),
		"",
		"escape.jsonnet",
	); err == nil || len(content) != 0 {
		t.Fatalf("escaping symlink was readable: content=%q err=%v", content, err)
	}
}

func TestWorkspaceImporterSearchRootValidationAndClose(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "vendor/lib.libsonnet", `1`)

	if _, err := NewWorkspaceImporter(
		root,
		WithSearchRoot("vendor", t.TempDir()),
	); err == nil {
		t.Fatal("expected a search root colliding with the workspace root to be rejected")
	}
	if _, err := NewWorkspaceImporter(
		root,
		WithSearchRoot("missing", filepath.Join(t.TempDir(), "absent")),
	); err == nil {
		t.Fatal("expected a missing search root directory to be rejected")
	}

	external := t.TempDir()
	writeTestFile(t, external, "lib.libsonnet", `2`)
	importer, err := NewWorkspaceImporter(root, WithSearchRoot("ext", external))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := importer.Import(context.Background(), "", "lib.libsonnet"); err != nil {
		t.Fatal(err)
	}

	if err := importer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := importer.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	// Close must release the search root as well as the workspace root.
	if _, _, err := importer.Import(
		context.Background(),
		"",
		"lib.libsonnet",
	); err == nil || errors.Is(err, ErrImportDenied) {
		t.Fatalf("closed search root import error = %v, want filesystem error", err)
	}
}

func TestWorkspaceImporterRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.jsonnet")
	if err := os.WriteFile(outside, []byte(`"secret"`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.jsonnet")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	importer, err := NewWorkspaceImporter(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := importer.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, content, err := importer.Import(
		context.Background(),
		"",
		"escape.jsonnet",
	); err == nil || len(content) != 0 {
		t.Fatalf("escaping symlink was readable: content=%q err=%v", content, err)
	}
}

func TestImporterValidationCancellationAndClose(t *testing.T) {
	mapImporter, err := NewMapImporter(map[string][]byte{
		"value.jsonnet": []byte(`42`),
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := mapImporter.Import(
		canceled,
		"",
		"value.jsonnet",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("MapImporter.Import() error = %v, want context cancellation", err)
	}

	if _, err := NewWorkspaceImporter(""); err == nil {
		t.Fatal("expected an empty workspace root to be rejected")
	}
	if _, err := NewWorkspaceImporter(t.TempDir(), nil); err == nil {
		t.Fatal("expected a nil workspace option to be rejected")
	}
	if _, err := NewWorkspaceImporter(
		t.TempDir(),
		WithLibraryPaths("../outside"),
	); err == nil {
		t.Fatal("expected an escaping library path to be rejected")
	}
	if _, err := NewWorkspaceImporter(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected a missing workspace root to be rejected")
	}

	var uninitialized *WorkspaceImporter
	if err := uninitialized.Close(); err != nil {
		t.Fatalf("nil WorkspaceImporter.Close() error = %v", err)
	}
	if _, _, err := uninitialized.Import(
		context.Background(),
		"",
		"value.jsonnet",
	); err == nil {
		t.Fatal("expected an uninitialized workspace importer to fail")
	}

	root := t.TempDir()
	writeTestFile(t, root, "value.jsonnet", `42`)
	workspace, err := NewWorkspaceImporter(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspace.Import(
		canceled,
		"",
		"value.jsonnet",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("WorkspaceImporter.Import() error = %v, want context cancellation", err)
	}
	if _, _, err := workspace.Import(
		context.Background(),
		"../main.jsonnet",
		"value.jsonnet",
	); !errors.Is(err, ErrImportDenied) {
		t.Fatalf("invalid importing path error = %v, want import denial", err)
	}
	if _, _, err := workspace.Import(
		context.Background(),
		"",
		string([]byte{'b', 'a', 'd', 0}),
	); !errors.Is(err, ErrImportDenied) {
		t.Fatalf("NUL import path error = %v, want import denial", err)
	}

	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("second WorkspaceImporter.Close() error = %v", err)
	}
	if _, _, err := workspace.Import(
		context.Background(),
		"",
		"value.jsonnet",
	); err == nil || errors.Is(err, ErrImportDenied) {
		t.Fatalf("closed workspace import error = %v, want filesystem error", err)
	}
}

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
