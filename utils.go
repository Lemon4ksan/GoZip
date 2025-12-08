// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"context"
	"io"
	"time"
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

// contextReader wraps an io.Reader to make it respect context cancellation.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *contextReader) Read(p []byte) (n int, err error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// Time conversion functions
func timeToMsDos(t time.Time) (dosDate uint16, dosTime uint16) {
	year := min(max(t.Year()-1980, 0), 127)
	month := uint16(t.Month())
	day := uint16(t.Day())
	hour := uint16(t.Hour())
	minute := uint16(t.Minute())
	second := uint16(t.Second())

	dosDate = uint16(year)<<9 | uint16(month)<<5 | day
	dosTime = uint16(hour)<<11 | uint16(minute)<<5 | uint16(second/2)
	return dosDate, dosTime
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

	return time.Date(year, time.Month(month), int(day), int(hour), int(minute), int(second), 0, time.UTC)
}

func hasPreciseTimestamps(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	_, w := metadata["LastWriteTime"]
	_, a := metadata["LastAccessTime"]
	_, c := metadata["CreationTime"]
	return w || a || c
}
