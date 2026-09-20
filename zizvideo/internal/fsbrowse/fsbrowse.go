// Package fsbrowse lists sub-directories for the admin directory picker.
// It deliberately returns directory names only: no file names, no sizes, no
// content, so even a stolen admin session cannot read files through it.
package fsbrowse

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zizdog/zizvideo/internal/domain"
)

// maxEntries caps one page; defaultLimit keeps first paint small.
const (
	maxEntries   = 1000
	defaultLimit = 200
)

// Entry is one child directory.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Subdirs  int    `json:"subdirs"`
}

// Result is the browse answer for one directory.
type Result struct {
	Path    string   `json:"path"`
	Parent  string   `json:"parent"`
	Entries []Entry  `json:"entries"`
	Total   int      `json:"total"`
	HasMore bool     `json:"has_more"`
	Offset  int      `json:"offset"`
	Starts  []string `json:"starts"`
}

// Validate checks the requested path without touching the filesystem.
func Validate(raw string) (string, error) {
	if raw == "" {
		return "", domain.ErrPathNotAbsolute
	}
	if strings.ContainsRune(raw, 0) {
		return "", domain.ErrPathNullByte
	}
	if !filepath.IsAbs(raw) {
		return "", domain.ErrPathNotAbsolute
	}
	clean := filepath.Clean(raw)
	if clean != raw {
		return "", domain.ErrPathNotClean
	}
	return clean, nil
}

// Browse lists the child directories of dir, dropping every non-directory
// entry. A symlink to a directory is offered (so /Volumes and /tmp work on
// macOS); registering it still re-validates the real path in the API.
// limit/offset page the listing so a huge directory cannot blow up a response.
func Browse(dir string, starts []string, offset, limit int) (*Result, error) {
	clean, err := Validate(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrPathNotExist
		}
		return nil, domain.ErrPathUnreadable
	}
	if !st.IsDir() {
		return nil, domain.ErrPathNotDir
	}
	f, err := os.Open(clean)
	if err != nil {
		return nil, domain.ErrPathUnreadable
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, domain.ErrPathUnreadable
	}
	sort.Strings(names)

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxEntries {
		limit = defaultLimit
	}
	dirs := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || name == "." || name == ".." {
			continue
		}
		child := filepath.Join(clean, name)
		if looksLikeDir(child) {
			dirs = append(dirs, name)
		}
	}
	res := &Result{Path: clean, Parent: parentOf(clean), Starts: starts,
		Entries: []Entry{}, Total: len(dirs), Offset: offset}
	if offset > len(dirs) {
		offset = len(dirs)
	}
	end := offset + limit
	if end > len(dirs) {
		end = len(dirs)
	}
	res.HasMore = end < len(dirs)
	for _, name := range dirs[offset:end] {
		child := filepath.Join(clean, name)
		readable, subdirs := scanChild(child)
		res.Entries = append(res.Entries, Entry{
			Name: name, Path: child, Readable: readable, Subdirs: subdirs,
		})
	}
	return res, nil
}

func looksLikeDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return true
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, terr := os.Stat(path)
		return terr == nil && target.IsDir()
	}
	return false
}

// scanChild reads the child once and returns readability plus its directory
// count; unreadable children come back as false/0 instead of an error.
func scanChild(dir string) (bool, int) {
	f, err := os.Open(dir)
	if err != nil {
		return false, 0
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return false, 0
	}
	n := 0
	for _, name := range names {
		info, ierr := os.Lstat(filepath.Join(dir, name))
		if ierr != nil {
			continue
		}
		if info.IsDir() || (info.Mode()&os.ModeSymlink != 0 && looksLikeDir(filepath.Join(dir, name))) {
			n++
		}
	}
	return true, n
}

func parentOf(dir string) string {
	parent := filepath.Dir(dir)
	if parent == dir {
		return ""
	}
	return parent
}
