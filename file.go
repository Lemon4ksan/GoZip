// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

	// StandardSizeLimit determines the maximum file size standard zip can store.
	// If this value is exceeded, the Zip64 format must be used.
	StandardSizeLimit = internal.MaxUint32

	// StandardEntriesLimit determines the maximum number of files standard zip can store.
	// If this value is exceeded, the Zip64 EOCD and locator must be used.
	StandardEntriesLimit = internal.MaxUint16

	// MaxStringLength determines the maximum length for filename and comment.
	MaxStringLength = internal.MaxUint16

	// ExtraFieldLimit determines the maximum extra field length the zip can hold.
	ExtraFieldLimit = internal.MaxUint16

	// Zip64ExtraFieldTag identifies the extra field that contains 64-bit size
	// and offset information for files exceeding 4GB limits.
	Zip64ExtraFieldTag = internal.Zip64ExtraFieldTag

	// NTFSFieldTag identifies the extra field that stores high-precision
	// NTFS file timestamps with 100-nanosecond resolution.
	NTFSFieldTag = internal.NTFSFieldTag

	// AESEncryptionTag identifies the extra field for WinZip AES encryption metadata,
	// including encryption strength and actual compression method.
	AESEncryptionTag = internal.AESEncryptionTag
)

// File represents a file entry within a ZIP archive, encapsulating both metadata
// and content access mechanisms. Each File object corresponds to one entry in the
// ZIP central directory and can represent either a regular file or a directory.
type File struct {
	mu sync.RWMutex // Protects name, config, extraField, metadata

	// Static
	isDir      bool                              // True if this entry represents a directory
	isImplicit bool                              // True if dir was created automatically.
	openFunc   func() (io.ReadCloser, error)     // Factory function for reading decompressed content
	srcFunc    func() (*io.SectionReader, error) // Factory function for reading compressed content
	srcConfig  FileConfig

	// Atomic
	uncompressedSize  int64  // Size of original content before compression in bytes
	compressedSize    int64  // Size of compressed data within archive in bytes
	localHeaderOffset int64  // Byte offset of this file's local header within archive
	crc32             uint32 // CRC-32 checksum of uncompressed data
	flags             uint16 // Internal flags state

	// Require mu
	name       string         // File path within the archive (using forward slashes)
	mode       fs.FileMode    // Unix-style file permissions and type bits
	modTime    time.Time      // File modification time (best available precision)
	hostSystem sys.HostSystem // Operating system that created the file (for attribute mapping)

	// Per-file configuration overriding archive defaults
	config        FileConfig
	metadata      map[string]interface{} // Platform-specific metadata (NTFS timestamps, etc.)
	extraField    map[uint16][]byte      // ZIP extra fields for extended functionality
	extraFieldRaw []byte                 // Raw extra field data

	extraParseOnce sync.Once // Sync for map initialization
	hasZip64Extra  bool      // True if 0x0001 tag is present in local header or cd
}

// newFileFromOS creates a File object from an already opened os.File handle.
func newFileFromOS(f *os.File) (*File, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: file cannot be nil", ErrFileEntry)
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var size int64
	if !stat.IsDir() {
		size = stat.Size()
	}

	return &File{
		name:             stat.Name(),
		uncompressedSize: size,
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

	isSymlink := info.Mode()&fs.ModeSymlink != 0
	var size int64
	var linkTarget string

	if isSymlink {
		linkTarget, err = os.Readlink(filePath)
		if err != nil {
			return nil, err
		}
		size = int64(len(linkTarget))
	} else if !info.IsDir() {
		size = info.Size()
	}

	f := &File{
		name:             info.Name(),
		uncompressedSize: size,
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

// newFileFromReader creates a File object from an arbitrary io.Reader source.
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

// newFileFromFS creates a File object from an fs.FS entry.
func newFileFromFS(fs fs.FS, filePath string, info fs.FileInfo) (*File, error) {
	size := info.Size()
	if info.IsDir() {
		size = 0
	}
	if size < 0 && size != SizeUnknown {
		return nil, fmt.Errorf("%w: size cannot be negative", ErrFileEntry)
	}

	return &File{
		name:             filePath,
		isDir:            info.IsDir(),
		uncompressedSize: size,
		modTime:          info.ModTime(),
		mode:             info.Mode(),
		hostSystem:       sys.DefaultHostSystem,
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return fs.Open(filePath)
		},
	}, nil
}

// Name returns the file's path within the ZIP archive.
func (f *File) Name() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.name
}

// IsDir returns true if the file represents a directory entry.
func (f *File) IsDir() bool { return f.isDir }

// IsImplicit returns true if the entry was created automatically and is not associated with a real directory.
func (f *File) IsImplicit() bool { return f.isImplicit }

// Mode returns underlying file attributes.
func (f *File) Mode() fs.FileMode { return f.mode }

// UncompressedSize returns the size of the original file content before compression.
func (f *File) UncompressedSize() int64 {
	return atomic.LoadInt64(&f.uncompressedSize)
}

// CompressedSize returns the size of the compressed data within the archive.
func (f *File) CompressedSize() int64 {
	return atomic.LoadInt64(&f.compressedSize)
}

// LocalHeaderOffset returns file entry offset inside the archive.
func (f *File) LocalHeaderOffset() int64 {
	return atomic.LoadInt64(&f.localHeaderOffset)
}

// CRC32 returns the CRC-32 checksum of the uncompressed file data.
func (f *File) CRC32() uint32 { return f.crc32 }

// Config returns archive file entry configuration.
func (f *File) Config() FileConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.config
}

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

// IsEncrypted checks wether the data from the original archive is encrypted.
func (f *File) IsEncrypted() bool {
	return f.srcConfig.EncryptionMethod != NotEncrypted
}

// Open returns an io.ReadCloser for reading the uncompressed content of the file.
//
// Behavior:
//   - If the file comes from an existing archive, the original compression
//     and encryption methods are preserved automatically.
//   - The file's Config is used ONLY to retrieve the decryption password.
//
// Errors:
//   - Returns [ErrPasswordMismatch] immediately if the provided password is incorrect
//     (for AES/ZipCrypto).
//   - The returned ReadCloser may return [ErrChecksum] during reading (typically at EOF)
//     or upon Close() if the data integrity check fails.
//   - Returns an error if the file is a directory or has no data source.
func (f *File) Open() (io.ReadCloser, error) {
	if f.openFunc == nil {
		return nil, errors.New("Open: data not available")
	}
	return f.openFunc()
}

// OpenRaw returns an [io.SectionReader] for reading the raw file content
// (compressed and/or encrypted) directly from the archive source.
// If the file is encrypted (AES), the reader includes Salt, PVV, and MAC bytes.
// Returns error if the file was created in memory (e.g. AddReader)
// and has not been written to disk yet.
//
// Use Cases:
//   - Efficiently copying files between archives without re-compression (Zero-Copy).
//   - Debugging compression headers or encryption metadata.
func (f *File) OpenRaw() (*io.SectionReader, error) {
	if f.srcFunc == nil {
		return nil, errors.New("OpenRaw: data not available (file not read from archive)")
	}
	return f.srcFunc()
}

// SetUncompressed size sets the file uncompressed size atomically.
func (f *File) SetUncompressedSize(size int64) {
	atomic.StoreInt64(&f.uncompressedSize, size)
}

// SetCompression replaces the compression method and level with the specified ones.
// This does not affect configuration for decompressing file from an existing archive.
func (f *File) SetCompression(method CompressionMethod, level int) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.config.CompressionMethod = method
	f.config.CompressionLevel = level
	return f
}

// SetEncryption replaces the encryption method and password with the specified ones.
// This does not affect configuration for decompressing file from an existing archive.
func (f *File) SetEncryption(method EncryptionMethod, pwd string) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.config.EncryptionMethod = method
	f.config.Password = pwd
	return f
}

// SetSourcePassword updates the password used to encrypt/decrypt this specific file.
// If current encryption is [NotEncrypted] it defaults to [AES256].
func (f *File) SetPassword(pwd string) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.config.EncryptionMethod == NotEncrypted {
		f.config.EncryptionMethod = AES256
	}
	f.config.Password = pwd
	return f
}

// SetSourcePassword updates the password used to read (decrypt) this specific file
// from the original archive in case if the archive-wide password was incorrect
// or if different files have different passwords.
func (f *File) SetSourcePassword(pwd string) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.srcConfig.Password = pwd
	return f
}

// DisableEncryption sets encryption method to [NotEncrypted] and removes the password for this file.
// This does not affect configuration for decompressing file from an existing archive.
func (f *File) DisableEncryption() *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.config.EncryptionMethod = NotEncrypted
	f.config.Password = ""
	return f
}

// SetMode updates the Unix-style file permission bits.
func (f *File) SetMode(mode fs.FileMode) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mode = mode
	return f
}

// SetModTime sets the file's last modification time.
func (f *File) SetModTime(modTime time.Time) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.modTime = modTime
	return f
}

func (f *File) SetComment(c string) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.config.Comment = c
	return f
}

// SetConfig applies a FileConfig to this file, overriding individual properties.
func (f *File) SetConfig(c FileConfig) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.isDir {
		f.config.CompressionMethod = c.CompressionMethod
		f.config.CompressionLevel = c.CompressionLevel
		f.config.EncryptionMethod = c.EncryptionMethod
		f.config.Password = c.Password
	}
	f.config.Comment = c.Comment
	return f
}

// SetOpenFunc replaces the function used to open the
// file's content and sets the size to [SizeUnknown].
func (f *File) SetOpenFunc(openFunc func() (io.ReadCloser, error)) *File {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.srcFunc = nil
	f.openFunc = openFunc
	f.uncompressedSize = SizeUnknown
	return f
}

// HasExtraField checks whether an extra field with the specified tag exists.
func (f *File) HasExtraField(tag uint16) bool {
	f.ensureExtraParsed()
	_, ok := f.extraField[tag]
	return ok
}

// GetExtraField retrieves the raw bytes of an extra field by its tag ID.
func (f *File) GetExtraField(tag uint16) []byte {
	f.ensureExtraParsed()
	return f.extraField[tag]
}

// SetExtraField adds or replaces an extra field entry for this file.
// Returns an error if adding the field would exceed the maximum extra field length.
func (f *File) SetExtraField(tag uint16, data []byte) error {
	f.ensureExtraParsed()

	// Check if the user has already provided a block with a tag (for protection purposes).
	var fullBlock []byte
	if len(data) >= 4 && binary.LittleEndian.Uint16(data[0:2]) == tag {
		fullBlock = data
	} else {
		// Wrap the payload in a Tag + Size header.
		fullBlock = make([]byte, 4+len(data))
		binary.LittleEndian.PutUint16(fullBlock[0:2], tag)
		binary.LittleEndian.PutUint16(fullBlock[2:4], uint16(len(data)))
		copy(fullBlock[4:], data)
	}

	currentLen := f.getExtraFieldLength()

	// If replacing, subtract the size of the old field
	if oldData, ok := f.extraField[tag]; ok {
		currentLen -= len(oldData)
	}

	if currentLen+len(data) > ExtraFieldLimit {
		return ErrExtraFieldTooLong
	}
	f.extraField[tag] = data
	f.extraFieldRaw = nil

	return nil
}

// RequiresZip64 determines whether this file requires ZIP64 format extensions.
func (f *File) RequiresZip64() bool {
	return f.CompressedSize() > StandardSizeLimit ||
		f.UncompressedSize() > StandardSizeLimit ||
		f.LocalHeaderOffset() > StandardSizeLimit
}

func (f *File) setCompressedSize(size int64) {
	atomic.StoreInt64(&f.compressedSize, size)
}

func (f *File) setCRC32(crc uint32) {
	atomic.StoreUint32(&f.crc32, crc)
}

// getExtraFieldLength calculates the total size of all extra field entries.
func (f *File) getExtraFieldLength() int {
	if f.extraField == nil {
		return len(f.extraFieldRaw)
	}
	var size int
	for _, entry := range f.extraField {
		size += len(entry)
	}
	return size
}

// entryName returns the filename as it appears in ZIP headers.
func (f *File) entryName() string {
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
		if f.config.Password != "" && f.config.Password != f.srcConfig.Password {
			return false
		}
	}
	if f.config.CompressionLevel != f.srcConfig.CompressionLevel {
		return false
	}
	return true
}

// ensureExtraParsed ensures that extraFiled is initialized and parsed.
func (f *File) ensureExtraParsed() {
	f.extraParseOnce.Do(func() {
		if f.extraField == nil {
			if len(f.extraFieldRaw) > 0 {
				f.extraField = internal.ParseExtraField(f.extraFieldRaw)
			} else {
				f.extraField = make(map[uint16][]byte)
			}
		}
	})
}

// FileSnapshot represents an immutable point-in-time copy of File metadata.
type FileSnapshot struct {
	file *File // internal reference

	Name       string
	IsDir      bool
	Mode       fs.FileMode
	ModTime    time.Time
	HostSystem sys.HostSystem

	// Configuration (Snapshot of FileConfig)
	Config FileConfig

	// State (Values read from atomics at the moment of snapshot)
	UncompressedSize  int64
	CompressedSize    int64
	LocalHeaderOffset int64
	CRC32             uint32
	Flags             uint16

	// Extra Fields (Deep copy or flattened representation)
	Metadata      map[string]interface{}
	ExtraField    map[uint16][]byte
	ExtraFieldRaw []byte
}

// Snapshot creates an immutable copy of the file's metadata.
func (f *File) Snapshot() *FileSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Ensure extra fields are parsed before copying
	f.ensureExtraParsed()

	// Deep copy extra fields to avoid race conditions on map access
	var extraCopy map[uint16][]byte
	if f.extraField != nil {
		extraCopy = make(map[uint16][]byte, len(f.extraField))
		for k, v := range f.extraField {
			// Copying the slice content is safer if the original slice is modified
			// though typically extra fields are replaced, not mutated in place.
			vCopy := make([]byte, len(v))
			copy(vCopy, v)
			extraCopy[k] = vCopy
		}
	}
	var metadataCopy map[string]interface{}
	if f.metadata != nil {
		metadataCopy = make(map[string]interface{}, len(f.metadata))
		for k, v := range f.metadata {
			metadataCopy[k] = v
		}
	}

	snap := &FileSnapshot{
		file:       f,
		Name:       f.name,
		IsDir:      f.isDir,
		Mode:       f.mode,
		ModTime:    f.modTime,
		HostSystem: f.hostSystem,

		Config: f.config,

		UncompressedSize:  atomic.LoadInt64(&f.uncompressedSize),
		CompressedSize:    atomic.LoadInt64(&f.compressedSize),
		LocalHeaderOffset: atomic.LoadInt64(&f.localHeaderOffset),
		CRC32:             atomic.LoadUint32(&f.crc32),
		Flags:             f.flags,

		Metadata:      metadataCopy,
		ExtraField:    extraCopy,
		ExtraFieldRaw: f.extraFieldRaw, // Copy slice header is enough if content is immutable
	}

	return snap
}

// RequiresZip64 checks if Zip64 format is needed based on the snapshot values.
func (s *FileSnapshot) RequiresZip64() bool {
	return s.CompressedSize > StandardSizeLimit ||
		s.UncompressedSize > StandardSizeLimit ||
		s.LocalHeaderOffset > StandardSizeLimit
}

// ResetEncodeOptions resets file encode options to default
func (s *FileSnapshot) ResetEncodeOptions() {
	s.Config.CompressionMethod = Store
	s.Config.CompressionLevel = 0
	s.Config.EncryptionMethod = NotEncrypted
	s.Config.Password = ""
}

// entryName returns the normalized filename for the header.
func (s *FileSnapshot) entryName() string {
	if s.IsDir {
		return s.Name + "/"
	}
	return s.Name
}

// zipHeaders is responsible for generating ZIP format headers from a FileHeaderSnapshot.
type zipHeaders struct {
	snap *FileSnapshot
}

// newZipHeaders accepts a snapshot instead of a raw File.
func newZipHeaders(snap *FileSnapshot) *zipHeaders {
	return &zipHeaders{snap: snap}
}

// LocalHeader generates the local file header that precedes the file data.
func (zh *zipHeaders) LocalHeader() internal.LocalFileHeader {
	dosDate, dosTime := timeToMsDos(zh.snap.ModTime)
	filename := zh.snap.entryName()
	localExtra := zh.buildLocalExtraData()

	return internal.LocalFileHeader{
		VersionNeededToExtract: zh.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.getFileBitFlag(),
		CompressionMethod:      zh.getCompressionMethod(),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.snap.CRC32,
		CompressedSize:         uint32(min(StandardSizeLimit, zh.snap.CompressedSize)),
		UncompressedSize:       uint32(min(StandardSizeLimit, zh.snap.UncompressedSize)),
		FilenameLength:         uint16(len(filename)),
		ExtraFieldLength:       uint16(len(localExtra)),
		Filename:               filename,
		ExtraField:             localExtra,
	}
}

// CentralDirEntry generates the central directory entry for this file.
func (zh *zipHeaders) CentralDirEntry() internal.CentralDirectory {
	dosDate, dosTime := timeToMsDos(zh.snap.ModTime)
	filename := zh.snap.entryName()

	var extraField []byte
	if zh.snap.ExtraField == nil {
		extraField = zh.snap.ExtraFieldRaw
	} else {
		extraField = zh.buildExtraFieldBytes()
	}

	return internal.CentralDirectory{
		VersionMadeBy:          zh.getVersionMadeBy(),
		VersionNeededToExtract: zh.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.getFileBitFlag(),
		CompressionMethod:      zh.getCompressionMethod(),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.snap.CRC32,
		CompressedSize:         uint32(min(StandardSizeLimit, zh.snap.CompressedSize)),
		UncompressedSize:       uint32(min(StandardSizeLimit, zh.snap.UncompressedSize)),
		FilenameLength:         uint16(len(filename)),
		ExtraFieldLength:       uint16(len(extraField)),
		FileCommentLength:      uint16(len(zh.snap.Config.Comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: zh.getExternalFileAttributes(),
		LocalHeaderOffset:      uint32(min(StandardSizeLimit, zh.snap.LocalHeaderOffset)),
		Filename:               filename,
		ExtraField:             extraField,
		Comment:                zh.snap.Config.Comment,
	}
}

func (zh *zipHeaders) getVersionNeededToExtract() uint16 {
	if zh.snap.Config.CompressionMethod == LZMA {
		return 63
	}
	if zh.snap.Config.EncryptionMethod == AES256 {
		return 51
	}
	if zh.snap.Config.CompressionMethod == BZIP2 {
		return 46
	}
	if zh.snap.RequiresZip64() {
		return 45
	}
	if zh.snap.Config.CompressionMethod == Deflate64 {
		return 21
	}
	if zh.snap.Config.CompressionMethod == Deflate {
		return 20
	}
	if zh.snap.IsDir || strings.Contains(zh.snap.Name, "/") {
		return 20
	}
	if zh.snap.Config.EncryptionMethod == ZipCrypto {
		return 20
	}
	return 10
}

func (zh *zipHeaders) getVersionMadeBy() uint16 {
	fs := zh.snap.HostSystem
	if fs == sys.HostSystemNTFS {
		fs = sys.HostSystemFAT
	}
	return uint16(fs)<<8 | LatestZipVersion
}

func (zh *zipHeaders) getFileBitFlag() uint16 {
	flag := zh.snap.Flags

	if zh.snap.Config.EncryptionMethod != NotEncrypted {
		flag |= 0x1
	}

	if zh.snap.Config.CompressionMethod == Deflate && zh.snap.UncompressedSize != 0 {
		flag |= zh.getCompressionLevelBits()
	}

	flag |= 0x800 // UTF-8 flag
	return flag
}

func (zh *zipHeaders) getCompressionMethod() uint16 {
	if zh.snap.UncompressedSize == 0 {
		return uint16(Store)
	}
	if zh.snap.Config.EncryptionMethod == AES256 {
		return winZipAESMarker
	}
	return uint16(zh.snap.Config.CompressionMethod)
}

func (zh *zipHeaders) getExternalFileAttributes() uint32 {
	var externalAttrs uint32

	switch zh.snap.HostSystem {
	case sys.HostSystemUNIX, sys.HostSystemDarwin:
		mode := uint32(zh.snap.Mode & fs.ModePerm)
		switch {
		case zh.snap.IsDir:
			mode |= sys.S_IFDIR
		case zh.snap.Mode&fs.ModeSymlink != 0:
			mode |= sys.S_IFLNK
		default:
			mode |= sys.S_IFREG
		}
		externalAttrs = mode << 16

	case sys.HostSystemFAT, sys.HostSystemNTFS:
		if zh.snap.IsDir {
			externalAttrs |= 0x10
		} else {
			externalAttrs |= 0x20
		}
		if zh.snap.Mode&0200 == 0 {
			externalAttrs |= 0x01 // ReadOnly
		}
	}
	return externalAttrs
}

func (zh *zipHeaders) getCompressionLevelBits() uint16 {
	level := zh.snap.Config.CompressionLevel
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

	if zh.snap.UncompressedSize > StandardSizeLimit || zh.snap.CompressedSize > StandardSizeLimit {
		buf = append(buf, internal.EncodeZip64LocalExtraField(zh.snap.UncompressedSize, zh.snap.CompressedSize)...)
	}

	if zh.snap.Config.EncryptionMethod == AES256 {
		buf = append(buf, internal.EncodeAESExtraField(uint16(zh.snap.Config.CompressionMethod))...)
	}

	return buf
}

func (zh *zipHeaders) buildExtraFieldBytes() []byte {
	// Works with snapshot's pre-copied map
	if len(zh.snap.ExtraField) == 0 {
		return nil
	}

	// Deterministic sorting is still needed for binary stability
	keys := make([]uint16, 0, len(zh.snap.ExtraField))
	for key := range zh.snap.ExtraField {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var buf []byte
	for _, key := range keys {
		buf = append(buf, zh.snap.ExtraField[key]...)
	}
	return buf
}
