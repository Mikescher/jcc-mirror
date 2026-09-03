//go:build linux || darwin

package engine

import "syscall"

// statfs is the free-space syscall. Bsize is a different width on the two
// platforms, hence the conversion.
func statfs(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	block := int64(st.Bsize)
	return int64(st.Bavail) * block, int64(st.Blocks) * block, nil
}
