//go:build windows

// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

import (
	"os"
	"syscall"
)

const DefaultHostSystem = HostSystemFAT

func GetFileMetadata(stat os.FileInfo) map[string]interface{} {
	metadata := make(map[string]interface{})
	if s, ok := stat.Sys().(*syscall.Win32FileAttributeData); ok {
		metadata["LastWriteTime"] = uint64(s.LastWriteTime.HighDateTime)<<32 | uint64(s.LastWriteTime.LowDateTime)
		metadata["LastAccessTime"] = uint64(s.LastAccessTime.HighDateTime)<<32 | uint64(s.LastAccessTime.LowDateTime)
		metadata["CreationTime"] = uint64(s.CreationTime.HighDateTime)<<32 | uint64(s.CreationTime.LowDateTime)
		metadata["FileAttributes"] = s.FileAttributes
	}
	return metadata
}
