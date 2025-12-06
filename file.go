// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"

	"github.com/lemon4ksan/gozip/internal"
	"github.com/lemon4ksan/gozip/internal/sys"
)

// Compression method indicates AES256 encryption.
// The actual compression method is stored in extra field.
const winZipAESMarker = 99

// Constants defining ZIP format structure and special tag values
const (
	// LatestZipVersion represents the maximum ZIP specification version supported
	// by this implementation. Version 63 corresponds to ZIP 6.3 specification.
	LatestZipVersion uint16 = 63

	// Zip64ExtraFieldTag identifies the extra field that contains 64-bit size
	// and offset information for files exceeding 4GB limits.
	Zip64ExtraFieldTag uint16 = 0x0001

	// NTFSFieldTag identifies the extra field that stores high-precision
	// NTFS file timestamps with 100-nanosecond resolution.
	NTFSFieldTag uint16 = 0x000A

	// AESEncryptionTag identifies the extra field for WinZip AES encryption metadata,
	// including encryption strength and actual compression method.
	AESEncryptionTag uint16 = 0x9901
)

// File represents a file entry within a ZIP archive, encapsulating both metadata
// and content access mechanisms. Each File object corresponds to one entry in the
// ZIP central directory and can represent either a regular file or a directory.
type File struct {
	name  string      // File path within the archive (using forward slashes)
	isDir bool        // True if this entry represents a directory
	mode  fs.FileMode // Unix-style file permissions and type bits

	openFunc         func() (io.ReadCloser, error) // Factory function for reading original content
	uncompressedSize int64                         // Size of original content before compression in bytes
	compressedSize   int64                         // Size of compressed data within archive in bytes
	crc32            uint32                        // CRC-32 checksum of uncompressed data

	// Per-file configuration overriding archive defaults
	config FileConfig

	localHeaderOffset int64          // Byte offset of this file's local header within archive
	hostSystem        sys.HostSystem // Operating system that created the file (for attribute mapping)

	modTime    time.Time              // File modification time (best available precision)
	metadata   map[string]interface{} // Platform-specific metadata (NTFS timestamps, etc.)
	extraField map[uint16][]byte      // ZIP extra fields for extended functionality
}

// newFileFromOS creates a File object from an already opened [os.File] handle.
// This method extracts metadata from the file descriptor and creates a reusable
// reader that seeks to the beginning on each Open() call. The caller remains
// responsible for closing the original file.
func newFileFromOS(f *os.File) (*File, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: file cannot be nil", ErrFileEntry)
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var uncompressedSize int64
	if !stat.IsDir() {
		uncompressedSize = stat.Size()
	}

	return &File{
		name:             stat.Name(),
		uncompressedSize: uncompressedSize,
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		mode:             stat.Mode(),
		metadata:         sys.GetFileMetadata(stat),
		hostSystem:       sys.GetHostSystem(f.Fd()),
		extraField:       make(map[uint16][]byte),
		// Create a closure that attempts to seek to start before reading
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(f, 0, stat.Size())), nil
		},
	}, nil
}

// newFileFromPath creates a File object by opening the file at the given path.
// The file is opened twice: once for metadata extraction and later for content
// reading. This ensures consistent metadata even if the file changes between
// addition and compression.
func newFileFromPath(filePath string) (*File, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var uncompressedSize int64
	if !stat.IsDir() {
		uncompressedSize = stat.Size()
	}

	return &File{
		name:             stat.Name(),
		uncompressedSize: uncompressedSize,
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		mode:             stat.Mode(),
		metadata:         sys.GetFileMetadata(stat),
		hostSystem:       sys.GetHostSystem(f.Fd()),
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return os.Open(filePath)
		},
	}, nil
}

// newFileFromReader creates a File object from an arbitrary [io.Reader] source.
func newFileFromReader(src io.Reader, name string, size int64) (*File, error) {
	if src == nil {
		return nil, fmt.Errorf("%w: reader cannot be nil", ErrFileEntry)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: filename cannot be empty", ErrFileEntry)
	}
	if size < 0 && size != SizeUnknown {
		return nil, fmt.Errorf("%w: size cannot be negative", ErrFileEntry)
	}

	return &File{
		name:             name,
		uncompressedSize: size,
		modTime:          time.Now(),
		hostSystem:       sys.GetHostSystemByOS(),
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(src), nil
		},
	}, nil
}

// newDirectoryFile creates a File object representing a directory entry.
// Directory entries contain no file data but preserve metadata like permissions,
// timestamps, and hierarchical structure within the archive.
func newDirectoryFile(name string) (*File, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: directory name cannot be empty", ErrFileEntry)
	}

	return &File{
		name:       name,
		isDir:      true,
		mode:       0755 | fs.ModeDir,
		hostSystem: sys.GetHostSystemByOS(),
		modTime:    time.Now(),
		extraField: make(map[uint16][]byte),
	}, nil
}

// Name returns the file's path within the ZIP archive.
// For directories, this does not include the trailing slash.
func (f *File) Name() string { return f.name }

// IsDir returns true if the file represents a directory entry.
func (f *File) IsDir() bool { return f.isDir }

// UncompressedSize returns the size of the original file content before compression.
// Returns sizeUnknown (-1) for files created from io.Reader with unknown size.
func (f *File) UncompressedSize() int64 { return f.uncompressedSize }

// CompressedSize returns the size of the compressed data within the archive.
// This value is only valid after compression has been performed.
func (f *File) CompressedSize() int64 { return f.compressedSize }

// CRC32 returns the CRC-32 checksum of the uncompressed file data.
// This value is used for data integrity verification during extraction.
func (f *File) CRC32() uint32 { return f.crc32 }

// ModTime returns the file's last modification timestamp.
func (f *File) ModTime() time.Time { return f.modTime }

// Open returns a ReadCloser for reading the original, uncompressed file content.
// The returned reader should be closed after use to release any held resources.
// For files created from os.File or file paths, this opens a fresh file handle.
func (f *File) Open() (io.ReadCloser, error) { return f.openFunc() }

// HasExtraField checks whether an extra field with the specified tag exists.
func (f *File) HasExtraField(tag uint16) bool { _, ok := f.extraField[tag]; return ok }

// GetExtraField retrieves the raw bytes of an extra field by its tag ID.
// Returns nil if no extra field with the given tag exists. The returned slice
// includes the 4-byte header (tag + size) followed by the field data.
func (f *File) GetExtraField(tag uint16) []byte { return f.extraField[tag] }

// SetConfig applies a FileConfig to this file, overriding individual properties.
// If config.Name is non-empty, it replaces the current filename. Compression
// and encryption settings are only applied to regular files (not directories).
func (f *File) SetConfig(config FileConfig) {
	if config.Name != "" {
		f.name = config.Name
	}
	if !f.isDir {
		f.config.CompressionMethod = config.CompressionMethod
		f.config.CompressionLevel = config.CompressionLevel
		f.config.EncryptionMethod = config.EncryptionMethod
		f.config.Password = config.Password
	}
	f.config.Comment = config.Comment
}

// SetOpenFunc replaces the internal function used to open the file's content.
// This allows customizing how file data is read, which is useful for advanced
// scenarios like streaming from network sources or applying transformations.
func (f *File) SetOpenFunc(openFunc func() (io.ReadCloser, error)) { f.openFunc = openFunc }

// SetExtraField adds or replaces an extra field entry for this file.
// The data parameter must include the 4-byte header (tag + size) followed by
// the field content. Returns an error if the tag already exists or if adding
// the field would exceed the maximum extra field length (65535 bytes).
func (f *File) SetExtraField(tag uint16, data []byte) error {
	if !f.HasExtraField(tag) && f.getExtraFieldLength()+len(data) > math.MaxUint16 {
		return ErrExtraFieldTooLong
	}
	f.extraField[tag] = data
	return nil
}

// RequiresZip64 determines whether this file requires ZIP64 format extensions.
// ZIP64 is needed when any of the file's dimensions exceed 32-bit limits
// (4GB for sizes, 4GB-1 for offsets). This affects header format and extra fields.
func (f *File) RequiresZip64() bool {
	return f.compressedSize > math.MaxUint32 ||
		f.uncompressedSize > math.MaxUint32 ||
		f.localHeaderOffset > math.MaxUint32
}

// getExtraFieldLength calculates the total size of all extra field entries.
// This includes both headers and data portions, used for header field population.
func (f *File) getExtraFieldLength() int {
	var size int
	for _, entry := range f.extraField {
		size += len(entry)
	}
	return size
}

// getFilename returns the filename as it appears in ZIP headers.
// For directories, this includes a trailing slash as required by the ZIP format.
func (f *File) getFilename() string {
	if f.isDir {
		return f.name + "/"
	}
	return f.name
}

// zipHeaders is responsible for generating ZIP format headers from File metadata.
// It encapsulates the logic for creating both local file headers and central
// directory entries, handling platform differences and format extensions.
type zipHeaders struct {
	file *File
}

// newZipHeaders creates a new zipHeaders instance for the given file.
func newZipHeaders(f *File) *zipHeaders {
	return &zipHeaders{file: f}
}

// LocalHeader generates the local file header that precedes the file data
// in the ZIP archive. This header contains information needed to extract
// the file, including compression method, sizes, and timestamps.
func (zh *zipHeaders) LocalHeader() internal.LocalFileHeader {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)
	filename := zh.file.getFilename()
	localExtra := zh.buildLocalExtraData()

	return internal.LocalFileHeader{
		VersionNeededToExtract: zh.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.getFileBitFlag(),
		CompressionMethod:      zh.getCompressionMethod(),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.file.crc32,
		CompressedSize:         uint32(min(math.MaxUint32, zh.file.compressedSize)),
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         uint16(len(filename)),
		ExtraFieldLength:       uint16(len(localExtra)),
		Filename:               filename,
		ExtraField:             localExtra,
	}
}

// CentralDirEntry generates the central directory entry for this file.
// This entry appears in the archive's central directory and contains
// comprehensive metadata, including file comment and external attributes.
func (zh *zipHeaders) CentralDirEntry() internal.CentralDirectory {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)
	filename := zh.file.getFilename()

	return internal.CentralDirectory{
		VersionMadeBy:          zh.getVersionMadeBy(),
		VersionNeededToExtract: zh.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.getFileBitFlag(),
		CompressionMethod:      zh.getCompressionMethod(),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.file.crc32,
		CompressedSize:         uint32(min(math.MaxUint32, zh.file.compressedSize)),
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         uint16(len(filename)),
		ExtraFieldLength:       uint16(zh.file.getExtraFieldLength()),
		FileCommentLength:      uint16(len(zh.file.config.Comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: zh.getExternalFileAttributes(),
		LocalHeaderOffset:      uint32(min(math.MaxUint32, zh.file.localHeaderOffset)),
		Filename:               filename,
		ExtraField:             zh.file.extraField,
		Comment:                zh.file.config.Comment,
	}
}

// getVersionNeededToExtract determines the minimum ZIP specification version
// required to correctly extract this file. Higher version numbers indicate
// use of advanced features like encryption, compression algorithms, or ZIP64.
func (zh *zipHeaders) getVersionNeededToExtract() uint16 {
	if zh.file.config.CompressionMethod == LZMA {
		return 63
	}
	if zh.file.config.EncryptionMethod == AES256 {
		return 51
	}
	if zh.file.config.CompressionMethod == BZIP2 {
		return 46
	}
	if zh.file.RequiresZip64() {
		return 45
	}
	if zh.file.config.CompressionMethod == Deflate64 {
		return 21
	}
	if zh.file.config.CompressionMethod == Deflated {
		return 20
	}
	if zh.file.isDir || strings.Contains(zh.file.name, "/") {
		return 20
	}
	if zh.file.config.EncryptionMethod == ZipCrypto {
		return 20
	}
	return 10
}

// getVersionMadeBy constructs the "version made by" field indicating both
// the creating host system and the ZIP specification version used.
// The high byte represents the host system, low byte the ZIP version.
func (zh *zipHeaders) getVersionMadeBy() uint16 {
	fs := zh.file.hostSystem
	// Normalize NTFS to FAT for broader compatibility
	if fs == sys.HostSystemNTFS {
		fs = sys.HostSystemFAT
	}
	return uint16(fs)<<8 | LatestZipVersion
}

// getFileBitFlag constructs the general purpose bit flag field that encodes
// various file characteristics like encryption status and compression options.
func (zh *zipHeaders) getFileBitFlag() uint16 {
	var flag uint16

	// Bit 0: encrypted file
	if zh.file.config.EncryptionMethod != NotEncrypted {
		flag |= 0x1
	}

	// Bits 1-2: compression options (for DEFLATE only)
	if zh.file.config.CompressionMethod == Deflated {
		flag |= zh.getCompressionLevelBits()
	}

	// Always set Bit 11 (Language encoding flag / EFS)
	// This indicates that Filename and Comment are encoded in UTF-8.
	// Go strings are always UTF-8, so this is technically always correct
	// and ensures compatibility with modern archivers (WinRAR, 7-Zip, macOS).
	flag |= 0x800

	return flag
}

// getCompressionMethod returns the compression method code for ZIP headers.
// For AES-encrypted files, returns the special marker value indicating that
// the actual compression method is stored in the AES extra field.
func (zh *zipHeaders) getCompressionMethod() uint16 {
	if zh.file.config.EncryptionMethod == AES256 {
		return winZipAESMarker
	}
	return uint16(zh.file.config.CompressionMethod)
}

// getExternalFileAttributes converts platform-specific file attributes to
// the ZIP external file attributes field format. This mapping varies by
// host system (Unix vs DOS/Windows) and preserves permissions, file types,
// and special attributes.
func (zh *zipHeaders) getExternalFileAttributes() uint32 {
	var externalAttrs uint32

	switch zh.file.hostSystem {
	case sys.HostSystemUNIX, sys.HostSystemDarwin:
		// Unix systems: store mode in high 16 bits
		mode := uint32(zh.file.mode & fs.ModePerm)

		switch {
		case zh.file.isDir:
			mode |= sys.S_IFDIR
		case zh.file.mode&fs.ModeSymlink != 0:
			mode |= sys.S_IFLNK
		default:
			mode |= sys.S_IFREG
		}

		externalAttrs = mode << 16

	case sys.HostSystemFAT, sys.HostSystemNTFS:
		// DOS/Windows systems: use attribute bits
		if zh.file.isDir {
			externalAttrs |= 0x10 // DOS Directory
		} else {
			externalAttrs |= 0x20 // DOS Archive
		}
		if zh.file.mode&0200 == 0 {
			externalAttrs |= 0x01 // DOS ReadOnly
		}
	}

	return externalAttrs
}

// getCompressionLevelBits encodes the DEFLATE compression
// level into the general purpose bit flag bits 1 and 2
func (zh *zipHeaders) getCompressionLevelBits() uint16 {
	level := zh.file.config.CompressionLevel
	if level == 0 {
		level = DeflateNormal
	}

	switch level {
	case DeflateSuperFast:
		return 0x0006
	case DeflateFast:
		return 0x0004
	case DeflateMaximum:
		return 0x0002
	default: // DeflateNormal
		return 0x0000
	}
}

// buildLocalExtraData constructs the Extra Field specifically for the Local File Header.
func (zh *zipHeaders) buildLocalExtraData() []byte {
	var buf []byte

	if zh.file.uncompressedSize > math.MaxUint32 || zh.file.compressedSize > math.MaxUint32 {
		buf = append(buf, encodeZip64LocalExtraField(zh.file)...)
	}

	// 2. AES Encryption
	if zh.file.config.EncryptionMethod == AES256 {
		buf = append(buf, encodeAESExtraField(zh.file)...)
	}

	return buf
}
