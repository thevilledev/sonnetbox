package securejsonnet

import (
	"context"
	"errors"
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

	for _, invalid := range []string{"", "/etc/passwd", "../secret", "a/../b", "./a", "a//b", `a\b`} {
		if _, err := NewMapImporter(map[string][]byte{invalid: nil}); err == nil {
			t.Errorf("expected %q to be rejected", invalid)
		}
	}

	_, _, err = importer.Import(context.Background(), "", "missing.jsonnet")
	if !errors.Is(err, errImportDenied) {
		t.Fatalf("expected import denial, got %v", err)
	}
}
