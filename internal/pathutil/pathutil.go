// Package pathutil has small path helpers shared by the other packages.
package pathutil

import (
	"path/filepath"
	"strings"
)

// Canon returns the absolute path of p with every symlink resolved, so two
// spellings of the same directory (/tmp vs /private/tmp) compare equal.
func Canon(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Within reports whether p is root or inside it. Both must be clean absolute
// paths.
func Within(p, root string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}
