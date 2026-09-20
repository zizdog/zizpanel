// Package media validates filesystem paths and runs the incremental scanner.
package media

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/govideo/internal/domain"
)

// Within reports whether path is root or sits below it.
func Within(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// ValidateLibraryPath enforces every documented rule for a library root:
// absolute, filepath.Clean-stable, existing directory, readable, and inside an
// allow root both before and after symlink resolution.
func ValidateLibraryPath(allowRoots []string, raw string) (string, error) {
	if raw == "" {
		return "", domain.ErrPathNotAbsolute
	}
	if strings.ContainsRune(raw, 0) {
		return "", domain.ErrPathNullByte
	}
	if !filepath.IsAbs(raw) {
		return "", domain.ErrPathNotAbsolute
	}
	// A path with ./ or // or a trailing slash is not the canonical spelling.
	if filepath.Clean(raw) != raw {
		return "", domain.ErrPathNotClean
	}
	roots, err := resolvedRoots(allowRoots)
	if err != nil {
		return "", err
	}
	// Resolve first: macOS aliases such as /tmp -> /private/tmp must compare
	// equal on both sides, while a symlink out of the roots must still fail.
	real := raw
	if r, rerr := filepath.EvalSymlinks(raw); rerr == nil {
		real = r
	}
	if !insideAny(real, roots) {
		if insideAny(raw, roots) {
			return "", domain.ErrPathEscapes
		}
		return "", domain.ErrPathNotAllowed
	}
	st, err := os.Lstat(raw)
	if err != nil {
		if os.IsNotExist(err) {
			return "", domain.ErrPathNotExist
		}
		return "", domain.ErrPathUnreadable
	}
	if !st.IsDir() {
		return "", domain.ErrPathNotDir
	}
	if err := checkReadableDir(raw); err != nil {
		return "", err
	}
	return real, nil
}

// resolvedRoots resolves each allow root, tolerating roots that do not exist yet.
func resolvedRoots(allowRoots []string) ([]string, error) {
	out := make([]string, 0, len(allowRoots))
	for _, r := range allowRoots {
		if !filepath.IsAbs(r) {
			return nil, domain.ErrPathNotAllowed
		}
		clean := filepath.Clean(r)
		out = append(out, clean)
		if real, err := filepath.EvalSymlinks(clean); err == nil && real != clean {
			out = append(out, real)
		}
	}
	if len(out) == 0 {
		return nil, domain.ErrPathNotAllowed
	}
	return out, nil
}

func insideAny(path string, roots []string) bool {
	for _, r := range roots {
		if Within(path, r) {
			return true
		}
	}
	return false
}

func checkReadableDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return domain.ErrPathUnreadable
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && err != io.EOF {
		return domain.ErrPathUnreadable
	}
	return nil
}

// ValidateMediaFile checks a discovered file against its library root and the
// allow roots. It is called before any open, including for /stream.
func ValidateMediaFile(allowRoots []string, libraryRoot, path string) (string, error) {
	if path == "" {
		return "", domain.ErrPathNotAbsolute
	}
	if strings.ContainsRune(path, 0) {
		return "", domain.ErrPathNullByte
	}
	if !filepath.IsAbs(path) {
		return "", domain.ErrPathNotAbsolute
	}
	if filepath.Clean(path) != path {
		return "", domain.ErrPathNotClean
	}
	roots, err := resolvedRoots(allowRoots)
	if err != nil {
		return "", err
	}
	if !insideAny(path, roots) {
		return "", domain.ErrPathNotAllowed
	}
	libReal, err := filepath.EvalSymlinks(libraryRoot)
	if err != nil {
		return "", domain.ErrPathNotExist
	}
	if !Within(path, libraryRoot) && !Within(path, libReal) {
		return "", domain.ErrPathEscapes
	}
	// Resolve the file's own directory: a symlink pointing outside the library
	// must be rejected even though its own path looks fine.
	dirReal, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", domain.ErrPathNotExist
	}
	full := filepath.Join(dirReal, filepath.Base(path))
	if !Within(full, libReal) && !Within(full, libraryRoot) {
		return "", domain.ErrPathEscapes
	}
	if !insideAny(full, roots) {
		return "", domain.ErrPathEscapes
	}
	target, err := filepath.EvalSymlinks(path)
	if err == nil && !insideAny(target, roots) {
		return "", domain.ErrPathEscapes
	}
	return path, nil
}
