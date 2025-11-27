package gozip

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"
)

// sizeUnknown applies to uncompressed size of files made from io.Reader
const sizeUnknown int64 = -1

// Constants for ZIP format
const (
	LatestZipVersion   uint16 = 63
	Zip64ExtraFieldTag uint16 = 0x0001
	NTFSFieldTag       uint16 = 0x000A
	AESEncryptionTag   uint16 = 0x9901
)

// file represents a file to be compressed and added to a ZIP archive
type file struct {
	// Basic file identification
	name  string
	isDir bool
	mode  fs.FileMode

	// File content and source
	openFunc         func() (io.ReadCloser, error)
	uncompressedSize int64
	compressedSize   int64
	crc32            uint32

	// Configuration
	config FileConfig

	// ZIP archive structure
	localHeaderOffset int64
	hostSystem        HostSystem

	// Metadata and timestamps
	modTime    time.Time
	metadata   map[string]interface{}
	extraField map[uint16][]byte
}

// newFileFromOS creates a file from an [os.File]
func newFileFromOS(f *os.File) (*file, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	uncompressedSize := stat.Size()
	if stat.IsDir() {
		uncompressedSize = 0
	}

	return &file{
		name:             stat.Name(),
		uncompressedSize: uncompressedSize,
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		mode:             stat.Mode(),
		metadata:         getFileMetadata(stat),
		hostSystem:       getHostSystem(f.Fd()),
		extraField:       make(map[uint16][]byte),
		// Create a closure that attempts to seek to start before reading
		openFunc: func() (io.ReadCloser, error) {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seek failed (stream?): %w", err)
			}
			return io.NopCloser(f), nil
		},
	}, nil
}

// newFileFromPath creates a file by opening file at given filePath
func newFileFromPath(filePath string) (*file, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file for metadata: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("get file stats: %w", err)
	}
	uncompressedSize := stat.Size()
	if stat.IsDir() {
		uncompressedSize = 0
	}

	return &file{
		name:             stat.Name(),
		uncompressedSize: uncompressedSize,
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		mode:             stat.Mode(),
		metadata:         getFileMetadata(stat),
		hostSystem:       getHostSystem(f.Fd()),
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return os.Open(filePath)
		},
	}, nil
}

// newFileFromReader creates a file from an [io.Reader]
func newFileFromReader(src io.Reader, name string) (*file, error) {
	return &file{
		name:             name,
		uncompressedSize: sizeUnknown,
		modTime:          time.Now(),
		hostSystem:       getHostSystemByOS(),
		extraField:       make(map[uint16][]byte),
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(src), nil
		},
	}, nil
}

// newDirectoryFile creates a file representing a directory
func newDirectoryFile(name string) (*file, error) {
	if name == "" {
		return nil, errors.New("directory name cannot be empty")
	}
	return &file{
		name:       name,
		isDir:      true,
		mode:       0755 | fs.ModeDir,
		hostSystem: getHostSystemByOS(),
		modTime:    time.Now(),
		extraField: make(map[uint16][]byte),
	}, nil
}

func (f *file) Name() string            { return f.name }
func (f *file) IsDir() bool             { return f.isDir }
func (f *file) UncompressedSize() int64 { return f.uncompressedSize }
func (f *file) CompressedSize() int64   { return f.compressedSize }
func (f *file) CRC32() uint32           { return f.crc32 }
func (f *file) ModTime() time.Time      { return f.modTime }

func (f *file) SetConfig(config FileConfig) {
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

func (f *file) SetOpenFunc(openFunc func() (io.ReadCloser, error)) { f.openFunc = openFunc }

// Open opens the decompressed file source for reading
func (f *file) Open() (io.ReadCloser, error) {
	return f.openFunc()
}

// RequiresZip64 checks whether zip64 extra field should be used for the file
func (f *file) RequiresZip64() bool {
	return f.compressedSize > math.MaxUint32 ||
		f.uncompressedSize > math.MaxUint32 ||
		f.localHeaderOffset > math.MaxUint32
}

// GetExtraField returns extra field entry with given tag
func (f *file) GetExtraField(tag uint16) []byte {
	return f.extraField[tag]
}

// HasExtraField checks if extra field with given tag exists
func (f *file) HasExtraField(tag uint16) bool {
	_, ok := f.extraField[tag]
	return ok
}

// AddExtraField adds an extra field entry to file. Data includes tag
func (f *file) AddExtraField(tag uint16, data []byte) error {
	if f.HasExtraField(tag) {
		return errors.New("entry with the same tag already exists")
	}
	if f.getExtraFieldLength()+len(data) > math.MaxUint16 {
		return errors.New("extra field length limit exceeded")
	}
	f.extraField[tag] = data
	return nil
}

// getExtraFieldLength returns the total length of all extra field entries
func (f *file) getExtraFieldLength() int {
	var size int
	for _, entry := range f.extraField {
		size += len(entry)
	}
	return size
}

// getFilenameLength returns the filename inside archive
func (f *file) getFilename() string {
	if f.isDir {
		return f.name + "/"
	}
	return f.name
}

// zipHeaders handles ZIP format header creation
type zipHeaders struct {
	file *file
}

// newZipHeaders creates a new ZipHeaders instance
func newZipHeaders(f *file) *zipHeaders {
	return &zipHeaders{file: f}
}

// LocalHeader creates a local file header
func (zh *zipHeaders) LocalHeader() localFileHeader {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)
	filename := zh.file.getFilename()
	var extraFieldLength uint16
	if zh.file.uncompressedSize > math.MaxUint32 {
		extraFieldLength += 8
	}
	if zh.file.compressedSize > math.MaxUint32 {
		extraFieldLength += 8
	}
	if extraFieldLength > 0 {
		extraFieldLength += 4 // Tag + Size
	}
	if zh.file.HasExtraField(AESEncryptionTag) {
		extraFieldLength += 11
	}

	return localFileHeader{
		VersionNeededToExtract: zh.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.getFileBitFlag(),
		CompressionMethod:      zh.getCompressionMethod(),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.file.crc32,
		CompressedSize:         uint32(min(math.MaxUint32, zh.file.compressedSize)),
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         uint16(len(filename)),
		ExtraFieldLength:       extraFieldLength,
		Filename:               filename,
	}
}

// CentralDirEntry creates a central directory entry
func (zh *zipHeaders) CentralDirEntry() centralDirectory {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)
	filename := zh.file.getFilename()

	return centralDirectory{
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

// getVersionNeededToExtract returns minimum ZIP version needed
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

// getVersionMadeBy returns version made by field
func (zh *zipHeaders) getVersionMadeBy() uint16 {
	fs := zh.file.hostSystem
	if fs == HostSystemNTFS {
		fs = HostSystemFAT
	}
	return uint16(fs)<<8 | LatestZipVersion
}

// getFileBitFlag returns general purpose bit flag
func (zh *zipHeaders) getFileBitFlag() uint16 {
	var flag uint16

	if zh.file.config.EncryptionMethod != NotEncrypted {
		flag |= 0x1
	}

	if zh.file.config.CompressionMethod == Deflated {
		flag |= zh.getCompressionLevelBits()
	}

	return flag
}

func (zh *zipHeaders) getCompressionMethod() uint16 {
	if zh.file.config.EncryptionMethod == AES256 {
		return winZipAESMarker
	}
	return uint16(zh.file.config.CompressionMethod)
}

// getExternalFileAttributes returns external file attributes
func (zh *zipHeaders) getExternalFileAttributes() uint32 {
	var externalAttrs uint32

	switch zh.file.hostSystem {
	case HostSystemUNIX, HostSystemDarwin:
		mode := uint32(zh.file.mode & fs.ModePerm)

		switch {
		case zh.file.isDir:
			mode |= s_IFDIR
		case zh.file.mode & fs.ModeSymlink != 0:
			mode |= s_IFLNK
		default:
			mode |= s_IFREG
		}

		externalAttrs = mode << 16

	case HostSystemFAT, HostSystemNTFS:
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

// getCompressionLevelBits returns compression level bits for DEFLATE compression
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
