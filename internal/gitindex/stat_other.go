//go:build !darwin && !linux

package gitindex

func lstat(path string) (fileStat, bool) { return fileStat{}, false }
