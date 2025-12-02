//go:build windows

// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetVolumeInfoByHandle = kernel32.NewProc("GetVolumeInformationByHandleW")
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

func GetHostSystem(fd uintptr) HostSystem {
	fsType := getWindowsFileSystem(fd)
	switch fsType {
	case FileSystemNTFS:
		return HostSystemNTFS
	case FileSystemFAT:
		return HostSystemFAT
	}
	return HostSystemNTFS // Default for Windows usually
}

func GetHostSystemByOS() HostSystem {
	return HostSystemNTFS
}

func getWindowsFileSystem(fd uintptr) FileSystemType {
	var (
		volumeName      [256]uint16
		fileSystemName  [256]uint16
		serialNumber    uint32
		maxComponentLen uint32
		fileSystemFlags uint32
	)

	ret, _, _ := procGetVolumeInfoByHandle.Call(
		fd,
		uintptr(unsafe.Pointer(&volumeName[0])),
		uintptr(len(volumeName)),
		uintptr(unsafe.Pointer(&serialNumber)),
		uintptr(unsafe.Pointer(&maxComponentLen)),
		uintptr(unsafe.Pointer(&fileSystemFlags)),
		uintptr(unsafe.Pointer(&fileSystemName[0])),
		uintptr(len(fileSystemName)),
	)

	if ret == 0 {
		return FileSystemUnknown
	}

	fsName := syscall.UTF16ToString(fileSystemName[:])
	switch fsName {
	case "NTFS":
		return FileSystemNTFS
	case "FAT", "FAT32", "exFAT":
		return FileSystemFAT
	default:
		return FileSystemUnknown
	}
}
