//go:build !windows

package gozip

import (
	"runtime"
)

// getHostSystem returns the HostSystem type. 
// On Unix we usually don't inspect the FD for filesystem type (like NTFS),
// we just report the OS type.
func getHostSystem(_ uintptr) HostSystem {
	return getHostSystemByOS()
}

func getHostSystemByOS() HostSystem {
	if runtime.GOOS == "darwin" {
		return HostSystemDarwin
	}
	return HostSystemUNIX
}

// unixNanoToWinFiletime converts Unix epoch time to Windows FILETIME (100ns ticks since 1601).
func unixNanoToWinFiletime(sec int64, nsec int64) uint64 {
	// Seconds between 1601-01-01 and 1970-01-01
	const intervalsDiff = 11644473600
	
	// Convert seconds to 100ns ticks
	ticks := (uint64(sec) + intervalsDiff) * 10000000
	
	// Add nanoseconds converted to 100ns ticks
	ticks += uint64(nsec) / 100
	
	return ticks
}