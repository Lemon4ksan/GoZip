package gozip

import "errors"

var (
	// ErrFormat is returned when the input is not a valid ZIP archive.
	ErrFormat = errors.New("zip: not a valid zip file")

	// ErrFileEntry is returned when an invalid argument is passed to File creation.
	ErrFileEntry = errors.New("zip: not a valid file entry")

	// ErrAlgorithm is returned when a compression algorithm is not supported.
	ErrAlgorithm = errors.New("unsupported compression algorithm")

	// ErrEncryption is returned when an encryption method is not supported.
	ErrEncryption = errors.New("unsupported encryption method")

	// ErrPasswordMismatch is returned when the provided password does not match.
	ErrPasswordMismatch = errors.New("zip: invalid password")

	// ErrChecksum is returned when reading a file checksum does not match.
	ErrChecksum = errors.New("zip: checksum error")

	// ErrSizeMismatch is returned when the uncompressed size does not match the header.
	ErrSizeMismatch = errors.New("zip: uncompressed size mismatch")

	// ErrFileNotFound is returned when the requested file is not found in the archive.
	ErrFileNotFound = errors.New("zip: file not found")

	// ErrInsecurePath is returned when a file path is invalid or attempts directory traversal (Zip Slip).
	ErrInsecurePath = errors.New("zip: insecure file path")

	// ErrDuplicateEntry is returned when attempting to add a file with a name that already exists.
	ErrDuplicateEntry = errors.New("zip: duplicate file name")

	// ErrFilenameTooLong is returned when a filename exceeds 65535 bytes.
	ErrFilenameTooLong = errors.New("zip: filename too long")

	// ErrCommentTooLong is returned when a file comment exceeds 65535 bytes.
	ErrCommentTooLong = errors.New("zip: comment too long")

	// ErrExtraFieldTooLong is returned when the total size of extra fields exceeds 65535 bytes.
	ErrExtraFieldTooLong = errors.New("zip: extra field too long")
)
