package gozip

import (
	"io"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// byteCountWriter counts bytes written to a writer
type byteCountWriter struct {
	dest         io.Writer
	bytesWritten int64
}

func (w *byteCountWriter) Write(p []byte) (int, error) {
	n, err := w.dest.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

// Time conversion functions
func timeToMsDos(t time.Time) (date uint16, time uint16) {
	year := max(t.Year()-1980, 0)
	if year > 127 {
		year = 127
	}

	month := uint16(t.Month())
	day := uint16(t.Day())
	hour := uint16(t.Hour())
	minute := uint16(t.Minute())
	second := uint16(t.Second())

	date = uint16(year)<<9 | uint16(month)<<5 | day
	time = uint16(hour)<<11 | uint16(minute)<<5 | uint16(second/2)

	return date, time
}

func msDosToTime(dosDate uint16, dosTime uint16) time.Time {
	day := dosDate & 0x1F
	month := (dosDate >> 5) & 0x0F
	year := int((dosDate>>9)&0x7F) + 1980
	second := (dosTime & 0x1F) * 2
	minute := (dosTime >> 5) & 0x3F
	hour := (dosTime >> 11) & 0x1F

	if month < 1 || month > 12 {
		month = 1
	}
	if day < 1 || day > 31 {
		day = 1
	}
	if hour > 23 {
		hour = 0
	}
	if minute > 59 {
		minute = 0
	}
	if second > 59 {
		second = 0
	}

	return time.Date(
		year,
		time.Month(month),
		int(day),
		int(hour),
		int(minute),
		int(second),
		0,
		time.UTC,
	)
}

// System helper functions
func getFileMetadata(stat os.FileInfo) map[string]interface{} {
	metadata := make(map[string]interface{})
	if s, ok := stat.Sys().(*syscall.Win32FileAttributeData); ok {
		metadata["LastWriteTime"] = uint64(s.LastWriteTime.Nanoseconds())/100 + 116444736000000000
		metadata["LastAccessTime"] = uint64(s.LastAccessTime.Nanoseconds())/100 + 116444736000000000
		metadata["CreationTime"] = uint64(s.CreationTime.Nanoseconds())/100 + 116444736000000000
		metadata["FileAttributes"] = s.FileAttributes
	}
	return metadata
}

func getFilePathFromFD(fd uintptr) string {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getFinalPathNameByHandle := kernel32.NewProc("GetFinalPathNameByHandleW")

	var path [1024]uint16
	ret, _, _ := getFinalPathNameByHandle.Call(
		fd,
		uintptr(unsafe.Pointer(&path[0])),
		uintptr(len(path)),
		0, // FILE_NAME_NORMALIZED
	)

	if ret == 0 || ret > uintptr(len(path)) {
		return ""
	}

	return syscall.UTF16ToString(path[:ret])
}

func hasPreciseTimestamps(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}

	if _, hasWriteTime := metadata["LastWriteTime"]; hasWriteTime {
		return true
	}
	if _, hasAccessTime := metadata["LastAccessTime"]; hasAccessTime {
		return true
	}
	if _, hasCreationTime := metadata["CreationTime"]; hasCreationTime {
		return true
	}

	return false
}

func getHostSystem(fd uintptr) HostSystem {
	fsType := getFileSystemFromFD(fd)

	switch fsType {
	case FileSystemNTFS:
		return HostSystemNTFS
	case FileSystemFAT:
		return HostSystemFAT
	case FileSystemEXT4, FileSystemZFS:
		return HostSystemUNIX
	case FileSystemAPFS, FileSystemHFSPlus:
		return HostSystemDarwin
	default:
		return getHostSystemByOS()
	}
}

func getHostSystemByOS() HostSystem {
	switch runtime.GOOS {
	case "windows":
		return HostSystemNTFS
	case "darwin":
		return HostSystemDarwin
	case "linux", "freebsd", "openbsd", "netbsd":
		return HostSystemUNIX
	case "zos":
		return HostSystemMVS
	default:
		return HostSystemFAT
	}
}

func getFileSystemFromFD(fd uintptr) FileSystemType {
	switch runtime.GOOS {
	case "windows":
		return getWindowsFileSystem(fd)
	default:
		return FileSystemUnknown
	}
}

func getWindowsFileSystem(fd uintptr) FileSystemType {
	var (
		volumeName      [256]uint16
		fileSystemName  [256]uint16
		serialNumber    uint32
		maxComponentLen uint32
		fileSystemFlags uint32
	)

	path := getFilePathFromFD(fd)
	if path == "" {
		return FileSystemUnknown
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getVolumeInformation := kernel32.NewProc("GetVolumeInformationW")

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return FileSystemUnknown
	}
	ret, _, _ := getVolumeInformation.Call(
		uintptr(unsafe.Pointer(pathPtr)),
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
