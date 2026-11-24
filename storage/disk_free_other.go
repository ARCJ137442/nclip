//go:build !linux && !windows

package storage

import "fmt"

// FreeBytes fails closed where this build has no supported filesystem probe.
func FreeBytes(path string) (int64, error) {
	return 0, fmt.Errorf("filesystem free-space probe is unsupported on this platform")
}

func FilesystemStats(path string) (int64, int64, error) {
	return 0, 0, fmt.Errorf("filesystem free-space probe is unsupported on this platform")
}
