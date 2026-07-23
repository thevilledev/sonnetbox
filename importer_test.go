package wasmnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
