package gitindex

import "syscall"

func lstat(path string) (fileStat, bool) {
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return fileStat{}, false
	}
	return fileStat{
		ctimeSec: st.Ctimespec.Sec, ctimeNsec: st.Ctimespec.Nsec,
		mtimeSec: st.Mtimespec.Sec, mtimeNsec: st.Mtimespec.Nsec,
		dev: uint64(st.Dev), ino: st.Ino, uid: st.Uid, gid: st.Gid,
		size: st.Size, mode: uint32(st.Mode) & sIFMT,
	}, true
}
