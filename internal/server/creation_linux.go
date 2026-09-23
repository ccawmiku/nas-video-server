//go:build linux

package server

import "golang.org/x/sys/unix"

func creationTime(path string) (int64, bool) {
	var st unix.Statx_t
	if unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BTIME, &st) != nil || st.Mask&unix.STATX_BTIME == 0 {
		return 0, false
	}
	return st.Btime.Sec, true
}
