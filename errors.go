// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"errors"
	"fmt"
	"io/fs"
)

// FileError wraps an error with context about a specific file entry.
type FileError struct {
	Op   string // Operation that failed (e.g., "open", "write", "extract")
	File *File  // The file entry that caused the error
	Err  error  // The actual error (sentinel error)
}

func (e *FileError) Error() string {
	if e.File == nil {
		return fmt.Sprintf("zip: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("zip: %s %s: %v", e.Op, e.File.Name(), e.Err)
}

// Unwrap allows errors.Is and errors.As to work with the underlying error.
func (e *FileError) Unwrap() error {
	return e.Err
}

var (
	// ErrFormat is returned when the input is not a valid ZIP archive.
	ErrFormat = errors.New("not a valid zip file")

	// ErrFileEntry is returned when an invalid argument is passed to File creation.
	ErrFileEntry = errors.New("not a valid file entry")

	// ErrAlgorithm is returned when a compression algorithm is not supported.
	ErrAlgorithm = errors.New("unsupported compression algorithm")

	// ErrPasswordMismatch is returned when the provided password does not match
	// or when a password is required but not provided.
	ErrPasswordMismatch = errors.New("invalid password")

	// ErrChecksum is returned when reading a file checksum does not match.
	ErrChecksum = errors.New("checksum error")

	// ErrSizeMismatch is returned when the uncompressed size does not match the header.
	ErrSizeMismatch = errors.New("uncompressed size mismatch")

	// ErrFileNotFound is returned when the requested file is not found in the archive.
	// It wraps fs.ErrNotExist so it can be checked with os.IsNotExist.
	ErrFileNotFound = fmt.Errorf("file not found: %w", fs.ErrNotExist)

	// ErrInsecurePath is returned when a file path is invalid or attempts directory traversal (Zip Slip).
	ErrInsecurePath = errors.New("insecure file path")

	// ErrDuplicateEntry is returned when attempting to add a file with a name that already exists.
	ErrDuplicateEntry = errors.New("duplicate file name")

	// ErrFilenameTooLong is returned when a filename exceeds 65535 bytes.
	ErrFilenameTooLong = errors.New("filename too long")

	// ErrCommentTooLong is returned when a file comment exceeds 65535 bytes.
	ErrCommentTooLong = errors.New("comment too long")

	// ErrExtraFieldTooLong is returned when the total size of extra fields exceeds 65535 bytes.
	ErrExtraFieldTooLong = errors.New("extra field too long")
)

// wrapErr is an internal helper for creating contextual errors.
// If f == nil, returns a formatted error.
// If f != nil, returns a FileError structure.
func wrapErr(op string, f *File, err error) error {
	if err == nil {
		return nil
	}
	if f == nil {
		return fmt.Errorf("zip: %s: %w", op, err)
	}
	return &FileError{
		Op:   op,
		File: f,
		Err:  err,
	}
}
