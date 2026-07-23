package securejsonnet

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

var errImportDenied = errors.New("import denied")

// MapImporter resolves imports from an immutable map of canonical virtual
// paths.
type MapImporter struct {
	files map[string][]byte
}

// NewMapImporter returns an immutable virtual-file importer. It validates all
// paths and copies all content before returning.
func NewMapImporter(files map[string][]byte) (*MapImporter, error) {
	copied := make(map[string][]byte, len(files))
	for name, content := range files {
		if err := validateVirtualPath(name); err != nil {
			return nil, &InvalidRequestError{Field: "import path", Err: err}
		}
		copied[name] = append([]byte(nil), content...)
	}
	return &MapImporter{files: copied}, nil
}

// Import implements Importer.
func (m *MapImporter) Import(
	ctx context.Context,
	importedFrom string,
	importedPath string,
) (string, []byte, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := validateVirtualPath(importedPath); err != nil {
		return "", nil, fmt.Errorf("%w: %v", errImportDenied, err)
	}
	base := ""
	if importedFrom != "" {
		if err := validateVirtualPath(importedFrom); err != nil {
			return "", nil, fmt.Errorf("%w: invalid importer path: %v", errImportDenied, err)
		}
		base = path.Dir(importedFrom)
		if base == "." {
			base = ""
		}
	}
	canonical := importedPath
	if base != "" {
		canonical = path.Join(base, importedPath)
	}
	if err := validateVirtualPath(canonical); err != nil {
		return "", nil, fmt.Errorf("%w: %v", errImportDenied, err)
	}
	content, ok := m.files[canonical]
	if !ok {
		return "", nil, fmt.Errorf("%w: %q is missing", errImportDenied, canonical)
	}
	return canonical, append([]byte(nil), content...), nil
}

func validateVirtualPath(name string) error {
	if name == "" {
		return errors.New("path is empty")
	}
	if !utf8.ValidString(name) {
		return errors.New("path is not valid UTF-8")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("path contains NUL")
	}
	if strings.Contains(name, "\\") {
		return errors.New("backslashes are not allowed")
	}
	if strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return errors.New("absolute paths are not allowed")
	}
	if path.Clean(name) != name {
		return errors.New("path is not canonical")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" {
			return errors.New("dot, traversal, and empty segments are not allowed")
		}
	}
	return nil
}
