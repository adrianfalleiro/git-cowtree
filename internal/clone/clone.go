// Package clone makes copy-on-write copies of files and directory trees: APFS
// clonefile on macOS, reflinks (FICLONE) on Linux filesystems that support
// them such as btrfs, XFS and bcachefs.
package clone

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrUnsupported is returned by Tree on platforms with no copy-on-write support.
var ErrUnsupported = errors.New("copy-on-write cloning not supported on this platform")

// Possible reports whether Tree can clone from directory src into directory
// dst, which needs both on the same volume and a filesystem that supports it.
// It checks by cloning a throwaway file.
func Possible(src, dst string) bool {
	probe := filepath.Join(src, fmt.Sprintf(".cowtree-probe.%d", os.Getpid()))
	// A distinct name, so src and dst may be the same directory.
	target := filepath.Join(dst, filepath.Base(probe)+".clone")
	defer os.Remove(probe)
	defer os.Remove(target)
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	return Tree(probe, target) == nil
}
