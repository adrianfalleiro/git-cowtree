package clone

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const ficlone = 0x40049409 // FICLONE ioctl (btrfs, XFS, bcachefs)

// Tree reflinks src (a file, symlink or directory tree) to dst, which must not
// exist. Linux has no directory clone, so files are reflinked one by one on a
// worker pool. File and symlink mtimes and permissions are preserved, since
// git's index relies on them.
func Tree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return cloneEntry(src, dst, info)
	}

	type job struct {
		src, dst string
		info     fs.FileInfo
	}
	jobs := make(chan job, 1024)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for range runtime.NumCPU() * 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := cloneEntry(j.src, j.dst, j.info); err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}

	var dirs []job
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, job{p, target, info})
			return os.MkdirAll(target, 0o700)
		}
		jobs <- job{p, target, info}
		return nil
	})
	close(jobs)
	wg.Wait()
	if walkErr != nil {
		return walkErr
	}
	if firstErr != nil {
		return firstErr
	}
	// Directory modes and times last, deepest first, so writes don't disturb them.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chmod(dirs[i].dst, dirs[i].info.Mode().Perm())
		os.Chtimes(dirs[i].dst, time.Now(), dirs[i].info.ModTime())
	}
	return nil
}

func cloneEntry(src, dst string, info fs.FileInfo) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return lchtimes(dst, info.ModTime())
	case !info.Mode().IsRegular():
		return nil // sockets, fifos, devices: skip
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd())
	var err2 error
	if errno != 0 {
		err2 = errno
	} else {
		// OpenFile's mode is filtered through the umask; match the source.
		err2 = out.Chmod(info.Mode().Perm())
	}
	if cerr := out.Close(); err2 == nil {
		err2 = cerr
	}
	if err2 != nil {
		os.Remove(dst)
		return err2
	}
	return os.Chtimes(dst, time.Now(), info.ModTime())
}

// lchtimes sets the mtime of path itself, not of what it links to; os.Chtimes
// follows symlinks.
func lchtimes(path string, mtime time.Time) error {
	const (
		atFdcwd           = -0x64         // AT_FDCWD
		utimeOmit         = (1 << 30) - 2 // UTIME_OMIT: leave atime alone
		atSymlinkNofollow = 0x100         // AT_SYMLINK_NOFOLLOW
	)
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	ts := [2]syscall.Timespec{{Nsec: utimeOmit}, syscall.NsecToTimespec(mtime.UnixNano())}
	atFd := atFdcwd
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(atFd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&ts[0])), atSymlinkNofollow, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
