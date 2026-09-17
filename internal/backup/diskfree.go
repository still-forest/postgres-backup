package backup

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// freeBytes returns the space available to an unprivileged writer at path.
func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}

	blocks, size := st.Bavail, uint64(st.Bsize)
	if size != 0 && blocks > math.MaxInt64/size {
		return math.MaxInt64, nil
	}
	free := blocks * size
	if free > math.MaxInt64 {
		return 0, fmt.Errorf("implausible free space reported for %s: %d bytes", path, free)
	}
	return int64(free), nil
}
