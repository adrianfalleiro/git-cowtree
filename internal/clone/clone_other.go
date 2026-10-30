//go:build !darwin && !linux

package clone

// Tree is unavailable here; callers fall back to a normal checkout.
func Tree(src, dst string) error { return ErrUnsupported }
