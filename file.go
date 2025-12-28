// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"encoding/binary"
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
	flags uint16      // Internal flags state

	openFunc func() (io.ReadCloser, error)     // Factory function for reading decompressed content
	srcFunc  func() (*io.SectionReader, error) // Factory function for reading original content

	uncompressedSize int64  // Size of original content before compression in bytes
	compressedSize   int64  // Size of compressed data within archive in bytes
	crc32            uint32 // CRC-32 checksum of uncompressed data

	// Per-file configuration overriding archive defaults
	config    FileConfig
	srcConfig FileConfig

	localHeaderOffset int64          // Byte offset of this file's local header within archive
	hostSystem        sys.HostSystem // Operating system that created the file (for attribute mapping)

	modTime    time.Time              // File modification time (best available precision)
	metadata   map[string]interface{} // Platform-specific metadata (NTFS timestamps, etc.)
	extraField map[uint16][]byte      // ZIP extra fields for extended functionality
}

// newFileFromOS creates a File object from an already opened [os.File] handle.
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
		hostSystem:       sys.DefaultHostSystem,
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			// NopCloser to prevent the caller from closing the original file handle
			return io.NopCloser(io.NewSectionReader(f, 0, stat.Size())), nil
		},
	}, nil
}

// newFileFromPath creates a File object by opening the file at the given path.
func newFileFromPath(filePath string) (*File, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, err
	}

	var uncompressedSize int64
	var isSymlink = info.Mode()&fs.ModeSymlink != 0
	var linkTarget string

	if isSymlink {
		linkTarget, err = os.Readlink(filePath)
		if err != nil {
			return nil, fmt.Errorf("read link: %w", err)
		}
		uncompressedSize = int64(len(linkTarget))
	} else if !info.IsDir() {
		uncompressedSize = info.Size()
	}

	f := &File{
		name:             info.Name(),
		uncompressedSize: uncompressedSize,
		modTime:          info.ModTime(),
		isDir:            info.IsDir(),
		mode:             info.Mode(),
		metadata:         sys.GetFileMetadata(info),
		hostSystem:       sys.DefaultHostSystem,
		extraField:       make(map[uint16][]byte),
	}

	if isSymlink {
		f.openFunc = func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(linkTarget)), nil
		}
	} else if !f.isDir {
		f.openFunc = func() (io.ReadCloser, error) {
			return os.Open(filePath)
		}
	}

	return f, nil
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
		mode:             0644,
		uncompressedSize: size,
		modTime:          time.Now(),
		hostSystem:       sys.DefaultHostSystem,
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(src), nil
		},
	}, nil
}

// newDirectoryFile creates a File object representing a directory entry.
func newDirectoryFile(name string) (*File, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: directory name cannot be empty", ErrFileEntry)
	}

	return &File{
		name:       name,
		isDir:      true,
		mode:       0755 | fs.ModeDir,
		hostSystem: sys.DefaultHostSystem,
		modTime:    time.Now(),
		extraField: make(map[uint16][]byte),
	}, nil
}

// Name returns the file's path within the ZIP archive.
func (f *File) Name() string { return f.name }

// IsDir returns true if the file represents a directory entry.
func (f *File) IsDir() bool { return f.isDir }

// Mode returns underlying file attributes.
func (f *File) Mode() fs.FileMode { return f.mode }

// UncompressedSize returns the size of the original file content before compression.
func (f *File) UncompressedSize() int64 { return f.uncompressedSize }

// CompressedSize returns the size of the compressed data within the archive.
func (f *File) CompressedSize() int64 { return f.compressedSize }

// CRC32 returns the CRC-32 checksum of the uncompressed file data.
func (f *File) CRC32() uint32 { return f.crc32 }

// Config returns archive file entry configuration.
func (f *File) Config() FileConfig { return f.config }

// HostSystem returns the system file was created in.
func (f *File) HostSystem() sys.HostSystem { return f.hostSystem }

// ModTime returns the file's last modification timestamp.
func (f *File) ModTime() time.Time { return f.modTime }

// FsTime returns the file timestamps (Modification, Access, Creation) if available.
func (f *File) FsTime() (mtime, atime, ctime time.Time) {
	if val, ok := f.metadata["LastWriteTime"]; ok {
		if t, ok := val.(uint64); ok {
			mtime = winFiletimeToTime(t)
		}
	}
	if val, ok := f.metadata["LastAccessTime"]; ok {
		if t, ok := val.(uint64); ok {
			atime = winFiletimeToTime(t)
		}
	}
	if val, ok := f.metadata["CreationTime"]; ok {
		if t, ok := val.(uint64); ok {
			ctime = winFiletimeToTime(t)
		}
	}
	return
}

// Open returns a ReadCloser for reading the original, uncompressed file content.
func (f *File) Open() (io.ReadCloser, error) { return f.openFunc() }

// Open returns a ReadCloser object for reading the original,
// uncompressed file content using the specified password.
func (f *File) OpenWithPassword(pwd string) (io.ReadCloser, error) {
	f.config.Password = pwd
	return f.openFunc()
}

// HasExtraField checks whether an extra field with the specified tag exists.
func (f *File) HasExtraField(tag uint16) bool { _, ok := f.extraField[tag]; return ok }

// GetExtraField retrieves the raw bytes of an extra field by its tag ID.
func (f *File) GetExtraField(tag uint16) []byte { return f.extraField[tag] }

// SetConfig applies a FileConfig to this file, overriding individual properties.
func (f *File) SetConfig(config FileConfig) {
	if !f.isDir {
		f.config.CompressionMethod = config.CompressionMethod
		f.config.CompressionLevel = config.CompressionLevel
		f.config.EncryptionMethod = config.EncryptionMethod
		f.config.Password = config.Password
	}
	f.config.Comment = config.Comment
}

// SetOpenFunc replaces the internal function used to open the file's content.
// Note that internal file sizes will be updated only after the archive is written.
func (f *File) SetOpenFunc(openFunc func() (io.ReadCloser, error)) {
	f.srcFunc = nil
	f.openFunc = openFunc
}

// SetExtraField adds or replaces an extra field entry for this file.
// Returns an error if adding the field would exceed the maximum extra field length.
func (f *File) SetExtraField(tag uint16, data []byte) error {
	currentLen := f.getExtraFieldLength()

	// If replacing, subtract the size of the old field
	if oldData, ok := f.extraField[tag]; ok {
		currentLen -= len(oldData)
	}

	if currentLen+len(data) > math.MaxUint16 {
		return ErrExtraFieldTooLong
	}
	f.extraField[tag] = data
	return nil
}

// RequiresZip64 determines whether this file requires ZIP64 format extensions.
func (f *File) RequiresZip64() bool {
	return f.compressedSize > math.MaxUint32 ||
		f.uncompressedSize > math.MaxUint32 ||
		f.localHeaderOffset > math.MaxUint32
}

// getExtraFieldLength calculates the total size of all extra field entries.
func (f *File) getExtraFieldLength() int {
	var size int
	for _, entry := range f.extraField {
		size += len(entry)
	}
	return size
}

// getFilename returns the filename as it appears in ZIP headers.
func (f *File) getFilename() string {
	if f.isDir {
		return f.name + "/"
	}
	return f.name
}

// shouldCopyRaw checks if we can optimize by copying raw compressed data directly.
func (f *File) shouldCopyRaw() bool {
	if f.srcFunc == nil {
		return false
	}
	// Configuration must match exactly to allow raw copy
	if f.config.CompressionMethod != f.srcConfig.CompressionMethod {
		return false
	}
	if f.config.EncryptionMethod != f.srcConfig.EncryptionMethod {
		return false
	}
	if f.config.EncryptionMethod != NotEncrypted {
		if f.config.Password != f.srcConfig.Password {
			return false
		}
	}
	return true
}

// zipHeaders is responsible for generating ZIP format headers from File metadata.
type zipHeaders struct {
	file *File
}

func newZipHeaders(f *File) *zipHeaders {
	return &zipHeaders{file: f}
}

// LocalHeader generates the local file header that precedes the file data.
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
	if zh.file.config.CompressionMethod == Deflate {
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

func (zh *zipHeaders) getVersionMadeBy() uint16 {
	fs := zh.file.hostSystem
	// Normalize NTFS to FAT for broader compatibility if needed
	if fs == sys.HostSystemNTFS {
		fs = sys.HostSystemFAT
	}
	return uint16(fs)<<8 | LatestZipVersion
}

func (zh *zipHeaders) getFileBitFlag() uint16 {
	flag := zh.file.flags

	if zh.file.config.EncryptionMethod != NotEncrypted {
		flag |= 0x1
	}

	if zh.file.config.CompressionMethod == Deflate {
		flag |= zh.getCompressionLevelBits()
	}

	// Always set Bit 11 (Language encoding flag / EFS)
	// This indicates that Filename and Comment are encoded in UTF-8.
	// Go strings are always UTF-8, so this is technically always correct
	// and ensures compatibility with modern archivers (WinRAR, 7-Zip, macOS).
	flag |= 0x800

	return flag
}

func (zh *zipHeaders) getCompressionMethod() uint16 {
	if zh.file.config.EncryptionMethod == AES256 {
		return winZipAESMarker
	}
	return uint16(zh.file.config.CompressionMethod)
}

func (zh *zipHeaders) getExternalFileAttributes() uint32 {
	var externalAttrs uint32

	switch zh.file.hostSystem {
	case sys.HostSystemUNIX, sys.HostSystemDarwin:
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
	default:
		return 0x0000
	}
}

func (zh *zipHeaders) buildLocalExtraData() []byte {
	var buf []byte

	// ZIP64: Only if dimensions exceed 32-bit (Local Header specific version)
	if zh.file.uncompressedSize > math.MaxUint32 || zh.file.compressedSize > math.MaxUint32 {
		buf = append(buf, encodeZip64LocalExtraField(zh.file)...)
	}

	// AES Encryption
	if zh.file.config.EncryptionMethod == AES256 {
		buf = append(buf, encodeAESExtraField(zh.file)...)
	}

	return buf
}

func encodeZip64LocalExtraField(f *File) []byte {
	// Fixed size: Tag(2) + Size(2) + Uncompressed(8) + Compressed(8) = 20 bytes
	data := make([]byte, 20)

	binary.LittleEndian.PutUint16(data[0:2], Zip64ExtraFieldTag)
	binary.LittleEndian.PutUint16(data[2:4], 16) // Size of payload
	binary.LittleEndian.PutUint64(data[4:12], uint64(f.uncompressedSize))
	binary.LittleEndian.PutUint64(data[12:20], uint64(f.compressedSize))

	return data
}
