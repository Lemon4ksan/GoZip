// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// byteCountWriter counts bytes written to a writer.
type byteCountWriter struct {
	dest         io.Writer
	bytesWritten int64
}

func (w *byteCountWriter) Write(p []byte) (int, error) {
	n, err := w.dest.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

type atomicCounterWriter struct {
	w     io.Writer
	count int64
}

func (acw *atomicCounterWriter) Write(p []byte) (int, error) {
	n, err := acw.w.Write(p)
	if n > 0 {
		atomic.AddInt64(&acw.count, int64(n))
	}
	return n, err
}

func (acw *atomicCounterWriter) Count() int64 {
	return atomic.LoadInt64(&acw.count)
}

type atomicCounterWriteSeeker struct {
	*atomicCounterWriter
	seeker io.Seeker
}

func (acw *atomicCounterWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	return acw.seeker.Seek(offset, whence)
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

// winFiletimeToTime converts Windows FILETIME (100ns ticks since 1601) to Go time.Time.
func winFiletimeToTime(ft uint64) time.Time {
	if ft == 0 {
		return time.Time{}
	}

	// 116444736000000000 is the number of 100ns intervals between
	// Jan 1, 1601 (UTC) and Jan 1, 1970 (UTC).
	const offset = 116444736000000000
	const ticksPerSecond = 10000000

	// Perform calculation in uint64 to avoid overflow issues with dates before 1970 during subtraction
	// Note: We assume the date is within valid Unix range for int64 conversion logic below.

	// Handle dates before 1970
	if ft < offset {
		diff := int64(offset - ft)
		seconds := -(diff / ticksPerSecond)
		nanos := -(diff % ticksPerSecond) * 100

		// Adjust if nanos is negative (standard time.Unix behavior handles this,
		// but explicit adjustment ensures correctness)
		if nanos < 0 {
			seconds--
			nanos += 1000000000
		}
		return time.Unix(seconds, nanos).UTC()
	}

	diff := ft - offset
	seconds := int64(diff / ticksPerSecond)
	nanos := int64(diff%ticksPerSecond) * 100

	return time.Unix(seconds, nanos).UTC()
}

// hasMeta checks if the string contains pattern matching characters.
func hasMeta(path string) bool {
	for _, c := range path {
		switch c {
		case '*', '?', '[', '\\':
			return true
		}
	}
	return false
}

// treeNode represents a node in the directory tree.
type treeNode struct {
	name     string
	isDir    bool
	children map[string]*treeNode
}

func newTreeNode(name string, isDir bool) *treeNode {
	return &treeNode{
		name:     name,
		isDir:    isDir,
		children: make(map[string]*treeNode),
	}
}

// generateTree converts a list of zip Files into a visual tree string.
func generateTree(files []*File) string {
	if len(files) == 0 {
		return ".\n└── (empty archive)"
	}

	// Build the tree structure from flat paths
	root := newTreeNode(".", true)

	for _, f := range files {
		// Clean path and split into components
		path := strings.Trim(f.Name(), "/")
		parts := strings.Split(path, "/")

		current := root
		for i, part := range parts {
			if part == "" {
				continue
			}

			// Determine if this part is a directory
			// It is a directory if it has children (i < len-1) OR if the file itself is a dir
			isDir := i < len(parts)-1 || f.IsDir()

			if _, exists := current.children[part]; !exists {
				current.children[part] = newTreeNode(part, isDir)
			}
			current = current.children[part]
		}
	}

	// Render the tree
	var sb strings.Builder
	sb.WriteString(".\n")
	renderTreeNode(root, "", &sb)

	return sb.String()
}

func renderTreeNode(node *treeNode, prefix string, sb *strings.Builder) {
	// Sort children keys to ensure deterministic output
	keys := make([]string, 0, len(node.children))
	for k := range node.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, name := range keys {
		child := node.children[name]
		isLast := i == len(keys)-1

		connector := "├── "
		if isLast {
			connector = "└── "
		}

		sb.WriteString(prefix)
		sb.WriteString(connector)
		sb.WriteString(name)
		if child.isDir {
			sb.WriteString("/")
		}
		sb.WriteString("\n")

		childPrefix := prefix
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}

		renderTreeNode(child, childPrefix, sb)
	}
}
