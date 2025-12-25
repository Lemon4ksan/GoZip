//go:build windows

// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

import (
	"os"
	"syscall"
)

func GetFileMetadata(stat os.FileInfo) map[string]interface{} {
	metadata := make(map[string]interface{})
	if s, ok := stat.Sys().(*syscall.Win32FileAttributeData); ok {
		metadata["LastWriteTime"] = uint64(s.LastWriteTime.Nanoseconds()/100) + 116444736000000000
		metadata["LastAccessTime"] = uint64(s.LastAccessTime.Nanoseconds()/100) + 116444736000000000
		metadata["CreationTime"] = uint64(s.CreationTime.Nanoseconds()/100) + 116444736000000000
		metadata["FileAttributes"] = s.FileAttributes
	}
	return metadata
}

func GetHostSystem(_ uintptr) HostSystem {
	return GetHostSystemByOS()
}

func GetHostSystemByOS() HostSystem {
	return HostSystemFAT
}
