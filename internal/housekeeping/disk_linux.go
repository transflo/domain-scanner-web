//go:build linux

package housekeeping

import "syscall"

// StatfsDisk reports the free and total bytes of the filesystem holding dir.
func StatfsDisk(dir string) (free, total int64, ok bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, 0, false
	}
	bs := int64(s.Bsize)
	return int64(s.Bavail) * bs, int64(s.Blocks) * bs, true
}
