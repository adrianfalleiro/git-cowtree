// Package gitindex edits the stat data cached in a git index file, so git
// trusts freshly cloned files instead of rehashing them.
package gitindex

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/parallel"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// fileStat is the subset of lstat(2) that git caches per index entry.
type fileStat struct {
	ctimeSec, ctimeNsec, mtimeSec, mtimeNsec int64
	dev, ino                                 uint64
	uid, gid                                 uint32
	size                                     int64
	mode                                     uint32 // S_IFMT bits only
}

const (
	sIFMT   = 0o170000
	gitlink = 0o160000
)

// RefreshStat rewrites the cached stat data (ctime, dev, inode, uid, gid) of
// every index entry whose file is an unchanged clone of what the entry
// describes: same mtime to the nanosecond, same size, same file type. Without
// this the first `git status` in a new worktree rehashes every tracked file,
// because the clones have new inodes and ctimes. It returns how many entries
// it updated.
//
// Entries modified at or after trustedBefore (when the source's index was
// written) are left alone: the source never verified those, so git must.
// Split indexes are left untouched.
func RefreshStat(worktree, indexPath, objectFormat string, trustedBefore time.Time) (updated int, err error) {
	defer report.Trace(time.Now(), "refresh index stat data")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return 0, err
	}
	newHash := sha1.New
	if objectFormat == "sha256" {
		newHash = sha256.New
	}
	be := binary.BigEndian
	trustSec, trustNsec := trustedBefore.Unix(), int64(trustedBefore.Nanosecond())

	type candidate struct {
		offset int
		path   string
	}
	var candidates []candidate
	off, err := walkEntries(data, newHash().Size(), func(e int, path []byte, flags, extFlags uint16) {
		mode := be.Uint32(data[e+24:])
		const assumeValid, skipWorktree = 0x8000, 0x4000
		if mode&sIFMT == gitlink || flags&assumeValid != 0 || extFlags&skipWorktree != 0 {
			return
		}
		mSec, mNsec := int64(be.Uint32(data[e+8:])), int64(be.Uint32(data[e+12:]))
		if mSec > trustSec || (mSec == trustSec && mNsec >= trustNsec) {
			return
		}
		candidates = append(candidates, candidate{e, filepath.Join(worktree, string(path))})
	})
	if err != nil {
		return 0, err
	}
	body := data[:len(data)-newHash().Size()]

	// lstat the clones in parallel: it's the first touch of their metadata, so
	// it's I/O-bound. Each chunk writes only its own entries' bytes.
	var chunks [][]candidate
	size := max(1, (len(candidates)+parallel.Workers-1)/parallel.Workers)
	for start := 0; start < len(candidates); start += size {
		chunks = append(chunks, candidates[start:min(start+size, len(candidates))])
	}
	var n atomic.Int64
	parallel.ForEach(chunks, func(part []candidate) {
		for _, c := range part {
			e := c.offset
			st, ok := lstat(c.path)
			if !ok || st.mode != be.Uint32(data[e+24:])&sIFMT ||
				uint32(st.mtimeSec) != be.Uint32(data[e+8:]) || uint32(st.mtimeNsec) != be.Uint32(data[e+12:]) ||
				uint32(st.size) != be.Uint32(data[e+36:]) {
				continue
			}
			be.PutUint32(data[e+0:], uint32(st.ctimeSec))
			be.PutUint32(data[e+4:], uint32(st.ctimeNsec))
			be.PutUint32(data[e+16:], uint32(st.dev))
			be.PutUint32(data[e+20:], uint32(st.ino))
			be.PutUint32(data[e+28:], st.uid)
			be.PutUint32(data[e+32:], st.gid)
			n.Add(1)
		}
	})

	// Keep extensions except caches that hold stat data or byte offsets we'd
	// otherwise have to recompute; git rebuilds them on demand.
	out := append([]byte{}, data[:off]...)
	for off+8 <= len(body) {
		sig := string(data[off : off+4])
		size := int(be.Uint32(data[off+4:]))
		if off+8+size > len(body) {
			return 0, errors.New("truncated index extension")
		}
		switch sig {
		case "link":
			return 0, nil // split index: leave it to git
		case "UNTR", "FSMN", "EOIE", "IEOT":
		default:
			out = append(out, data[off:off+8+size]...)
		}
		off += 8 + size
	}
	return int(n.Load()), writeIndex(indexPath, out, newHash)
}

// walkEntries calls fn with the offset, path and flags of every entry in the
// index data, and returns the offset of the first extension.
func walkEntries(data []byte, hashLen int, fn func(offset int, path []byte, flags, extFlags uint16)) (int, error) {
	if len(data) < 12+hashLen || string(data[:4]) != "DIRC" {
		return 0, errors.New("not a git index")
	}
	be := binary.BigEndian
	version := be.Uint32(data[4:])
	if version < 2 || version > 4 {
		return 0, errors.New("unsupported index version")
	}
	count := int(be.Uint32(data[8:]))
	body := data[:len(data)-hashLen]
	off := 12
	var prevPath []byte
	for range count {
		e := off
		pos := e + 40 + hashLen + 2
		if pos > len(body) {
			return 0, errors.New("truncated index")
		}
		flags := be.Uint16(data[e+40+hashLen:])
		var extFlags uint16
		if version >= 3 && flags&0x4000 != 0 {
			if pos+2 > len(body) {
				return 0, errors.New("truncated index")
			}
			extFlags = be.Uint16(data[pos:])
			pos += 2
		}

		var path []byte
		if version == 4 {
			strip, n := readOffsetVarint(body[pos:])
			if n == 0 || int(strip) > len(prevPath) {
				return 0, errors.New("bad v4 path")
			}
			pos += n
			end := bytes.IndexByte(body[pos:], 0)
			if end < 0 {
				return 0, errors.New("truncated index")
			}
			path = append(append([]byte{}, prevPath[:len(prevPath)-int(strip)]...), data[pos:pos+end]...)
			off = pos + end + 1
		} else {
			end := bytes.IndexByte(body[pos:], 0)
			if end < 0 {
				return 0, errors.New("truncated index")
			}
			path = data[pos : pos+end]
			off = e + (pos-e+end+8)&^7
		}
		prevPath = path
		fn(e, path, flags, extFlags)
	}
	return off, nil
}

// SharedIndexID returns the object id of the shared index that the split
// index at indexPath builds on (its sharedindex.<id> file sits next to it), or
// "" if the index isn't split.
func SharedIndexID(indexPath, objectFormat string) (string, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", err
	}
	hashLen := sha1.Size
	if objectFormat == "sha256" {
		hashLen = sha256.Size
	}
	off, err := walkEntries(data, hashLen, func(int, []byte, uint16, uint16) {})
	if err != nil {
		return "", err
	}
	body := data[:len(data)-hashLen]
	be := binary.BigEndian
	for off+8 <= len(body) {
		size := int(be.Uint32(data[off+4:]))
		if off+8+size > len(body) {
			return "", errors.New("truncated index extension")
		}
		if string(data[off:off+4]) == "link" && size >= hashLen {
			return hex.EncodeToString(data[off+8 : off+8+hashLen]), nil
		}
		off += 8 + size
	}
	return "", nil
}

// readOffsetVarint decodes git's "offset" varint (varint.c), used by index v4.
// It returns the value and the bytes consumed, or 0 bytes if b is truncated.
func readOffsetVarint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	val := uint64(c & 127)
	n := 1
	for c&128 != 0 {
		if n >= len(b) {
			return 0, 0
		}
		val++
		c = b[n]
		n++
		val = val<<7 + uint64(c&127)
	}
	return val, n
}

// writeIndex writes body plus its checksum through index.lock, like git does.
func writeIndex(indexPath string, body []byte, newHash func() hash.Hash) error {
	h := newHash()
	h.Write(body)
	lock := indexPath + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(h.Sum(body))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(lock)
		return err
	}
	return os.Rename(lock, indexPath)
}
