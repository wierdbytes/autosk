// Package pathcanon provides best-effort filesystem path canonicalisation
// shared by daemon project loading and worktree path derivation.
package pathcanon

import (
	"os"
	"path/filepath"
	"strings"
)

// Existing returns an absolute, clean, symlink-resolved path when possible.
// On case-insensitive filesystems it also rewrites existing path components to
// their directory-entry casing, so aliases that differ only by case collapse to
// one stable spelling. If the path cannot be resolved, Existing returns the
// absolute clean spelling so callers retain their previous fallback behavior.
func Existing(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canon := filepath.Clean(abs)
	prefix := canon
	var suffix []string
	for {
		if _, statErr := os.Stat(prefix); statErr == nil {
			break
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return canon, nil
		}
		suffix = append(suffix, filepath.Base(prefix))
		prefix = parent
	}
	if r, err := filepath.EvalSymlinks(prefix); err == nil {
		prefix = filepath.Clean(r)
	}
	if cased, err := withDirectoryEntryCase(prefix); err == nil {
		prefix = cased
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		prefix = filepath.Join(prefix, suffix[i])
	}
	return prefix, nil
}

// SameDir reports whether two directory paths refer to the same filesystem
// object. It prefers os.SameFile so case-insensitive aliases and symlinked
// spellings compare equal, then falls back to Existing + lexical comparison
// for paths that cannot be statted.
func SameDir(a, b string) bool {
	if ai, aerr := os.Stat(a); aerr == nil {
		if bi, berr := os.Stat(b); berr == nil {
			return os.SameFile(ai, bi)
		}
	}
	ca, aerr := Existing(a)
	if aerr != nil {
		ca = filepath.Clean(a)
	}
	cb, berr := Existing(b)
	if berr != nil {
		cb = filepath.Clean(b)
	}
	return ca == cb
}

func withDirectoryEntryCase(path string) (string, error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	sep := string(filepath.Separator)

	root := volume
	for strings.HasPrefix(rest, sep) {
		root += sep
		rest = strings.TrimPrefix(rest, sep)
		break
	}
	if root == "" {
		root = "."
	}
	if rest == "" || rest == "." {
		return root, nil
	}

	cur := root
	for part := range strings.SplitSeq(rest, sep) {
		if part == "" || part == "." {
			continue
		}
		entries, err := os.ReadDir(cur)
		if err != nil {
			return "", err
		}
		match := ""
		for _, entry := range entries {
			name := entry.Name()
			if name == part {
				match = name
				break
			}
			if match == "" && strings.EqualFold(name, part) {
				match = name
			}
		}
		if match == "" {
			return "", os.ErrNotExist
		}
		cur = filepath.Join(cur, match)
	}
	return cur, nil
}
