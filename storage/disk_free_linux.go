//go:build linux

package storage

import (
	"fmt"
	"math"
	"syscall"
)

// FreeBytes reports unprivileged available bytes on the filesystem containing path.
func FreeBytes(path string) (int64, error) {
	free, _, err := FilesystemStats(path)
	return free, err
}

func FilesystemStats(path string) (freeBytes, blockSize int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	blocks := uint64(stat.Bavail)
	blockSizeValue := uint64(stat.Frsize)
	if blockSizeValue == 0 {
		blockSizeValue = uint64(stat.Bsize)
	}
	if blockSizeValue != 0 && blocks > math.MaxInt64/blockSizeValue {
		return 0, 0, fmt.Errorf("filesystem free-space value overflows int64")
	}
	if blockSizeValue > math.MaxInt64 {
		return 0, 0, fmt.Errorf("filesystem block size overflows int64")
	}
	return int64(blocks * blockSizeValue), int64(blockSizeValue), nil
}
