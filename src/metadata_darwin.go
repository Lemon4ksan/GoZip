//go:build darwin || freebsd || netbsd || openbsd

package gozip

import (
	"os"
	"syscall"
)

func getFileMetadata(stat os.FileInfo) map[string]interface{} {
	s, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	meta := map[string]interface{}{
		"LastAccessTime": unixNanoToWinFiletime(int64(s.Atimespec.Sec), int64(s.Atimespec.Nsec)),
		"LastWriteTime":  unixNanoToWinFiletime(int64(s.Mtimespec.Sec), int64(s.Mtimespec.Nsec)),
	}

	meta["CreationTime"] = unixNanoToWinFiletime(int64(s.Birthtimespec.Sec), int64(s.Birthtimespec.Nsec))

	return meta
}