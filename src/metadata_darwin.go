//go:build darwin || freebsd || netbsd || openbsd

// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

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
