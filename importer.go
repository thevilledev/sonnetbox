package sonnetbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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
		copied[name] = bytes.Clone(content)
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
	return canonical, bytes.Clone(content), nil
}

// WorkspaceOption configures a WorkspaceImporter.
type WorkspaceOption func(*workspaceConfig) error

type workspaceConfig struct {
	searchPaths []declaredSearchPath
}

// declaredSearchPath records one search location before its root is opened.
// An empty mount denotes a library path inside the primary workspace root.
type declaredSearchPath struct {
	mount string
	path  string
}

// WithLibraryPaths adds virtual Jsonnet library paths inside the workspace
// root. They are searched in reverse declaration order after resolution
// relative to the importing file, matching go-jsonnet FileImporter precedence.
func WithLibraryPaths(paths ...string) WorkspaceOption {
	copied := slices.Clone(paths)
	return func(config *workspaceConfig) error {
		declared := make([]declaredSearchPath, 0, len(copied))
		for _, libraryPath := range copied {
			if err := validateVirtualPath(libraryPath); err != nil {
				return fmt.Errorf("invalid library path %q: %w", libraryPath, err)
			}
			declared = append(declared, declaredSearchPath{path: libraryPath})
		}
		config.searchPaths = append(config.searchPaths, declared...)
		return nil
	}
}

// WithSearchRoot grants one additional read-only host directory and addresses
// it in virtual paths by mountName, which must be a single path segment.
//
// This is the equivalent of a go-jsonnet FileImporter JPath that lives outside
// the workspace root. Each search root is a separate explicit grant with its
// own traversal-resistant root, so a Jsonnet program still cannot reach any
// directory the host did not name. Imports resolved in a search root report
// paths beneath mountName, which keeps two roots holding the same relative
// path distinguishable and makes a relative import from a file found in a
// search root resolve inside that same root.
//
// Search roots and library paths share one precedence list: every declaration
// is searched in reverse order, whichever option declared it.
func WithSearchRoot(mountName string, hostPath string) WorkspaceOption {
	return func(config *workspaceConfig) error {
		if err := validateMountName(mountName); err != nil {
			return fmt.Errorf("invalid search root name %q: %w", mountName, err)
		}
		if hostPath == "" {
			return fmt.Errorf("search root %q has an empty path", mountName)
		}
		for _, existing := range config.searchPaths {
			if existing.mount == mountName {
				return fmt.Errorf("search root %q is already declared", mountName)
			}
		}
		config.searchPaths = append(config.searchPaths, declaredSearchPath{
			mount: mountName,
			path:  hostPath,
		})
		return nil
	}
}

// workspaceRoot is one open read-only grant. The primary workspace root has an
// empty mount; a search root is addressed in virtual paths by its mount name.
type workspaceRoot struct {
	mount string
	root  *os.Root
}

// virtual maps a path inside the grant to the canonical path reported to the
// caller.
func (r workspaceRoot) virtual(relative string) string {
	if r.mount == "" {
		return relative
	}
	return r.mount + "/" + relative
}

// workspaceSearch is one location tried after the importing file's own
// directory.
type workspaceSearch struct {
	root workspaceRoot
	// directory is the virtual directory inside root to resolve against.
	directory string
}

// importCandidate is one resolved place to look for an import.
type importCandidate struct {
	root workspaceRoot
	// relative is the slash path inside root.
	relative string
	// virtual is the canonical path reported to the caller.
	virtual string
}

// WorkspaceImporter exposes read-only files beneath one host directory, plus
// any additional search roots. It prevents relative paths and symlinks from
// escaping every granted directory.
type WorkspaceImporter struct {
	primary   workspaceRoot
	mounts    []workspaceRoot
	searches  []workspaceSearch
	closeOnce sync.Once
	closeErr  error
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
	importer := &WorkspaceImporter{primary: workspaceRoot{root: root}}
	for _, declared := range config.searchPaths {
		if err := importer.addSearchPath(declared); err != nil {
			return nil, errors.Join(err, importer.Close())
		}
	}
	return importer, nil
}

// addSearchPath resolves one declared search location, opening a root when the
// declaration names a host directory outside the workspace.
func (w *WorkspaceImporter) addSearchPath(declared declaredSearchPath) error {
	if declared.mount == "" {
		w.searches = append(w.searches, workspaceSearch{
			root:      w.primary,
			directory: declared.path,
		})
		return nil
	}
	if err := w.checkMountIsFree(declared.mount); err != nil {
		return err
	}
	root, err := os.OpenRoot(declared.path)
	if err != nil {
		return fmt.Errorf("open Jsonnet search root %q: %w", declared.mount, err)
	}
	mount := workspaceRoot{mount: declared.mount, root: root}
	w.mounts = append(w.mounts, mount)
	w.searches = append(w.searches, workspaceSearch{root: mount})
	return nil
}

// checkMountIsFree refuses a mount name the workspace root already uses. The
// workspace root is searched first, so a shadowed search root could never
// serve an import and its virtual paths would be ambiguous.
func (w *WorkspaceImporter) checkMountIsFree(mount string) error {
	_, err := w.primary.root.Stat(mount)
	switch {
	case err == nil:
		return fmt.Errorf(
			"search root %q collides with an entry in the workspace root",
			mount,
		)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check search root name %q: %w", mount, err)
	}
}

// Close releases the workspace root and every search root. It is idempotent.
func (w *WorkspaceImporter) Close() error {
	if w == nil || w.primary.root == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		closeErrs := make([]error, 0, len(w.mounts)+1)
		closeErrs = append(closeErrs, w.primary.root.Close())
		for _, mount := range w.mounts {
			closeErrs = append(closeErrs, mount.root.Close())
		}
		w.closeErr = errors.Join(closeErrs...)
	})
	return w.closeErr
}

// Import implements Importer.
func (w *WorkspaceImporter) Import(
	ctx context.Context,
	importedFrom string,
	importedPath string,
) (string, []byte, error) {
	if w == nil || w.primary.root == nil {
		return "", nil, errors.New("workspace importer is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if importedFrom != "" {
		if err := validateVirtualPath(importedFrom); err != nil {
			return "", nil, fmt.Errorf("%w: invalid importing path: %w", ErrImportDenied, err)
		}
	}
	if err := validateImportPath(importedPath); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrImportDenied, err)
	}

	candidates := w.candidates(importedFrom, importedPath)
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate.virtual]; exists {
			continue
		}
		seen[candidate.virtual] = struct{}{}
		content, err := candidate.root.root.ReadFile(filepath.FromSlash(candidate.relative))
		if err == nil {
			return candidate.virtual, content, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, ctxErr
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("read workspace import %q: %w", candidate.virtual, err)
		}
	}
	return "", nil, fmt.Errorf("%w: %q is missing from the workspace", ErrImportDenied, importedPath)
}

// candidates lists the places to try in go-jsonnet FileImporter precedence
// order: the importing file's own directory first, then every declared search
// path in reverse declaration order.
func (w *WorkspaceImporter) candidates(
	importedFrom string,
	importedPath string,
) []importCandidate {
	owner, directory := w.locate(importedFrom)
	candidates := make([]importCandidate, 0, len(w.searches)+1)
	if candidate, ok := resolveCandidate(owner, directory, importedPath); ok {
		candidates = append(candidates, candidate)
	}
	for _, search := range slices.Backward(w.searches) {
		if candidate, ok := resolveCandidate(search.root, search.directory, importedPath); ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// locate reports the grant that owns importedFrom and the virtual directory
// inside it to resolve a relative import against. A file found in a search
// root resolves its relative imports inside that same root.
func (w *WorkspaceImporter) locate(importedFrom string) (workspaceRoot, string) {
	if importedFrom == "" {
		return w.primary, ""
	}
	for _, mount := range w.mounts {
		if rest, found := strings.CutPrefix(importedFrom, mount.mount+"/"); found {
			return mount, virtualDirectory(rest)
		}
	}
	return w.primary, virtualDirectory(importedFrom)
}

func resolveCandidate(
	root workspaceRoot,
	directory string,
	importedPath string,
) (importCandidate, bool) {
	relative, err := resolveAgainstVirtualDirectory(directory, importedPath)
	if err != nil {
		return importCandidate{}, false
	}
	return importCandidate{
		root:     root,
		relative: relative,
		virtual:  root.virtual(relative),
	}, true
}

func resolveVirtualImport(importedFrom, importedPath string) (string, error) {
	if importedFrom != "" {
		if err := validateVirtualPath(importedFrom); err != nil {
			return "", fmt.Errorf("%w: invalid importing path: %w", ErrImportDenied, err)
		}
	}
	if err := validateImportPath(importedPath); err != nil {
		return "", fmt.Errorf("%w: %w", ErrImportDenied, err)
	}
	canonical, err := resolveAgainstVirtualDirectory(
		virtualDirectory(importedFrom),
		importedPath,
	)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrImportDenied, err)
	}
	return canonical, nil
}

// virtualDirectory returns the directory holding name, empty at the root.
func virtualDirectory(name string) string {
	if name == "" {
		return ""
	}
	if directory := path.Dir(name); directory != "." {
		return directory
	}
	return ""
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
	return validateRelativeSlashPath(name)
}

func validateRelativeSlashPath(name string) error {
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
	if err := validateRelativeSlashPath(name); err != nil {
		return err
	}
	if cleaned := path.Clean(name); cleaned != name {
		return errors.New("path is not canonical")
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "." || part == ".." || part == "" {
			return errors.New("dot, traversal, and empty segments are not allowed")
		}
	}
	return nil
}

func validateMountName(name string) error {
	if err := validateVirtualPath(name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return errors.New("name must be a single path segment")
	}
	return nil
}
