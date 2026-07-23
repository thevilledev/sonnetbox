package securejsonnet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// ErrImportDenied can be returned by a custom Importer when a path is absent
// or rejected by policy.
var ErrImportDenied = errors.New("import denied")

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
	canonical, err := resolveVirtualImport(importedFrom, importedPath)
	if err != nil {
		return "", nil, err
	}
	content, ok := m.files[canonical]
	if !ok {
		return "", nil, fmt.Errorf("%w: %q is missing", ErrImportDenied, canonical)
	}
	return canonical, append([]byte(nil), content...), nil
}

// WorkspaceOption configures a WorkspaceImporter.
type WorkspaceOption func(*workspaceConfig) error

type workspaceConfig struct {
	libraryPaths []string
}

// WithLibraryPaths adds virtual Jsonnet library paths. They are searched in
// reverse order after resolution relative to the importing file, matching
// go-jsonnet FileImporter precedence.
func WithLibraryPaths(paths ...string) WorkspaceOption {
	copied := append([]string(nil), paths...)
	return func(config *workspaceConfig) error {
		for _, libraryPath := range copied {
			if err := validateVirtualPath(libraryPath); err != nil {
				return fmt.Errorf("invalid library path %q: %w", libraryPath, err)
			}
		}
		config.libraryPaths = append(config.libraryPaths, copied...)
		return nil
	}
}

// WorkspaceImporter exposes read-only files beneath one host directory. It
// prevents relative paths and symlinks from escaping that directory.
type WorkspaceImporter struct {
	root         *os.Root
	libraryPaths []string
	closeOnce    sync.Once
	closeErr     error
}

// NewWorkspaceImporter opens a traversal-resistant, read-only workspace root.
func NewWorkspaceImporter(
	rootPath string,
	options ...WorkspaceOption,
) (*WorkspaceImporter, error) {
	if rootPath == "" {
		return nil, &InvalidRequestError{
			Field: "workspace root",
			Err:   errors.New("path is empty"),
		}
	}
	config := workspaceConfig{}
	for _, option := range options {
		if option == nil {
			return nil, &InvalidRequestError{
				Field: "workspace option",
				Err:   errors.New("option is nil"),
			}
		}
		if err := option(&config); err != nil {
			return nil, &InvalidRequestError{Field: "workspace option", Err: err}
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open Jsonnet workspace: %w", err)
	}
	return &WorkspaceImporter{
		root:         root,
		libraryPaths: append([]string(nil), config.libraryPaths...),
	}, nil
}

// Close releases the workspace root. It is idempotent.
func (w *WorkspaceImporter) Close() error {
	if w == nil || w.root == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closeErr = w.root.Close()
	})
	return w.closeErr
}

// Import implements Importer.
func (w *WorkspaceImporter) Import(
	ctx context.Context,
	importedFrom string,
	importedPath string,
) (string, []byte, error) {
	if w == nil || w.root == nil {
		return "", nil, errors.New("workspace importer is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if importedFrom != "" {
		if err := validateVirtualPath(importedFrom); err != nil {
			return "", nil, fmt.Errorf("%w: invalid importing path: %v", ErrImportDenied, err)
		}
	}
	if err := validateImportPath(importedPath); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrImportDenied, err)
	}

	base := ""
	if importedFrom != "" {
		base = path.Dir(importedFrom)
		if base == "." {
			base = ""
		}
	}
	candidates := make([]string, 0, len(w.libraryPaths)+1)
	if candidate, err := resolveAgainstVirtualDirectory(base, importedPath); err == nil {
		candidates = append(candidates, candidate)
	}
	for i := len(w.libraryPaths) - 1; i >= 0; i-- {
		candidate, err := resolveAgainstVirtualDirectory(w.libraryPaths[i], importedPath)
		if err == nil {
			candidates = append(candidates, candidate)
		}
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		content, err := w.root.ReadFile(filepath.FromSlash(candidate))
		if err == nil {
			return candidate, content, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, ctxErr
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("read workspace import %q: %w", candidate, err)
		}
	}
	return "", nil, fmt.Errorf("%w: %q is missing from the workspace", ErrImportDenied, importedPath)
}

func resolveVirtualImport(importedFrom, importedPath string) (string, error) {
	if importedFrom != "" {
		if err := validateVirtualPath(importedFrom); err != nil {
			return "", fmt.Errorf("%w: invalid importing path: %v", ErrImportDenied, err)
		}
	}
	if err := validateImportPath(importedPath); err != nil {
		return "", fmt.Errorf("%w: %v", ErrImportDenied, err)
	}
	base := ""
	if importedFrom != "" {
		base = path.Dir(importedFrom)
		if base == "." {
			base = ""
		}
	}
	canonical, err := resolveAgainstVirtualDirectory(base, importedPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrImportDenied, err)
	}
	return canonical, nil
}

func resolveAgainstVirtualDirectory(directory, importedPath string) (string, error) {
	canonical := path.Clean(path.Join(directory, importedPath))
	if canonical == "." || canonical == ".." || strings.HasPrefix(canonical, "../") {
		return "", errors.New("path escapes the virtual root")
	}
	if err := validateVirtualPath(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func validateImportPath(name string) error {
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
	if filepath.VolumeName(filepath.FromSlash(name)) != "" {
		return errors.New("volume-qualified paths are not allowed")
	}
	return nil
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
	for part := range strings.SplitSeq(name, "/") {
		if part == "." || part == ".." || part == "" {
			return errors.New("dot, traversal, and empty segments are not allowed")
		}
	}
	return nil
}
