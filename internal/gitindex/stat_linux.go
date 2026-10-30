package gitindex

import "syscall"

func lstat(path string) (fileStat, bool) {
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return fileStat{}, false
	}
	return fileStat{
		ctimeSec: int64(st.Ctim.Sec), ctimeNsec: int64(st.Ctim.Nsec),
		mtimeSec: int64(st.Mtim.Sec), mtimeNsec: int64(st.Mtim.Nsec),
		dev: uint64(st.Dev), ino: uint64(st.Ino), uid: st.Uid, gid: st.Gid,
		size: st.Size, mode: uint32(st.Mode) & sIFMT,
	}, true
}
