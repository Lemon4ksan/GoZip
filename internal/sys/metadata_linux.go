//go:build linux

// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

import (
	"os"
	"syscall"
)

func GetFileMetadata(stat os.FileInfo) map[string]interface{} {
	s, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	return map[string]interface{}{
		// Atim - Access Time
		"LastAccessTime": unixNanoToWinFiletime(int64(s.Atim.Sec), int64(s.Atim.Nsec)),
		// Mtim - Modification Time
		"LastWriteTime": unixNanoToWinFiletime(int64(s.Mtim.Sec), int64(s.Mtim.Nsec)),

		// Linux syscall.Stat_t typically does not expose "BirthTime" (Creation Time).
		// Note: s.Ctim is "Change Time" (metadata change), NOT creation time.
	}
}
