//go:build windows

package storage

import (
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getDiskFreeSpaceProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceW")

func FreeBytes(path string) (int64, error) {
	free, _, err := FilesystemStats(path)
	return free, err
}

func FilesystemStats(path string) (freeBytes, blockSize int64, err error) {
	directory, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, &total, &totalFree); err != nil {
		return 0, 0, err
	}
	var sectorsPerCluster, bytesPerSector, freeClusters, totalClusters uint32
	result, _, callErr := getDiskFreeSpaceProc.Call(
		uintptr(unsafe.Pointer(directory)),
		uintptr(unsafe.Pointer(&sectorsPerCluster)),
		uintptr(unsafe.Pointer(&bytesPerSector)),
		uintptr(unsafe.Pointer(&freeClusters)),
		uintptr(unsafe.Pointer(&totalClusters)),
	)
	if result == 0 {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceW failed: %w", callErr)
	}
	clusterBytes := uint64(sectorsPerCluster) * uint64(bytesPerSector)
	if available > math.MaxInt64 || clusterBytes == 0 || clusterBytes > math.MaxInt64 {
		return 0, 0, fmt.Errorf("filesystem size value is outside supported range")
	}
	return int64(available), int64(clusterBytes), nil
}
