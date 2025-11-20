package gozip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"time"
)

// Constants for ZIP format
const (
	__LATEST_ZIP_VERSION     uint16 = 63
	__ZIP64_EXTRA_FIELD_ID   uint16 = 0x0001
	__NTFS_METADATA_FIELD_ID uint16 = 0x000A
)

// file represents a file to be compressed and added to a ZIP archive
type file struct {
	// Basic file identification
	name  string
	path  string
	isDir bool

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
	extraField []ExtraFieldEntry
}

// newFileFromOS creates a file from an [os.File]
func newFileFromOS(f *os.File) (*file, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}

	return &file{
		name:             stat.Name(),
		uncompressedSize: stat.Size(),
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		metadata:         getFileMetadata(stat),
		hostSystem:       getHostSystem(f.Fd()),
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

	hostSys := getHostSystem(f.Fd())
	metadata := getFileMetadata(stat)

	return &file{
		name:             stat.Name(),
		uncompressedSize: stat.Size(),
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		metadata:         metadata,
		hostSystem:       hostSys,
		openFunc: func() (io.ReadCloser, error) {
			return os.Open(filePath)
		},
	}, nil
}

// newFileFromReader creates a file from an [io.Reader]
func newFileFromReader(source io.Reader, name string) (*file, error) {
	return &file{
		name:       name,
		modTime:    time.Now(),
		hostSystem: getHostSystemByOS(),
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(source), nil
		},
	}, nil
}

// newDirectoryFile creates a file representing a directory
func newDirectoryFile(dirPath, dirName string) (*file, error) {
	if dirName == "" {
		return nil, errors.New("directory name cannot be empty")
	}
	return &file{
		name:       dirName,
		path:       dirPath,
		isDir:      true,
		hostSystem: getHostSystemByOS(),
		modTime:    time.Now(),
	}, nil
}

func (f *file) Name() string            { return f.name }
func (f *file) Path() string            { return f.path }
func (f *file) IsDir() bool             { return f.isDir }
func (f *file) UncompressedSize() int64 { return f.uncompressedSize }
func (f *file) CompressedSize() int64   { return f.compressedSize }
func (f *file) CRC32() uint32           { return f.crc32 }
func (f *file) ModTime() time.Time      { return f.modTime }

func (f *file) SetConfig(config FileConfig) {
	if f.isDir {
		f.config.Path = config.Path
		f.config.Comment = config.Comment
	} else {
		f.config = config
	}
}

func (f *file) SetOpenFunc(openFunc func() (io.ReadCloser, error)) { f.openFunc = openFunc }

// RequiresZip64 checks whether zip64 extra field should be used for the file
func (f *file) RequiresZip64() bool {
	return f.compressedSize > math.MaxUint32 ||
		f.uncompressedSize > math.MaxUint32 ||
		f.localHeaderOffset > math.MaxUint32
}

// normalizeEntry stores serialized path from config to struct
func (f *file) normalizeEntry() {
	if f.config.Name != "" {
		f.name = f.config.Name
	}
	if f.config.Path != "" {
		fullPath := path.Clean(strings.ReplaceAll(f.config.Path, "\\", "/"))
		if fullPath == "." || fullPath == "/" {
			fullPath = ""
		}

		f.path = fullPath
	}
}

// getExtraFieldLength returns the total length of all extra field entries
func (f *file) getExtraFieldLength() uint16 {
	var size uint16
	for _, entry := range f.extraField {
		size += uint16(len(entry.Data))
	}
	return size
}

// getFilenameLength returns the length of the filename in bytes
func (f *file) getFilenameLength() uint16 {
	filename := path.Join(f.path, f.name)
	if f.isDir && !strings.HasSuffix(filename, "/") {
		filename += "/"
	}
	return uint16(len(filename))
}

// fileAttributes handles file attribute calculations
type fileAttributes struct {
	file *file
}

// NewFileAttributes creates a new fileAttributes instance
func NewFileAttributes(f *file) *fileAttributes {
	return &fileAttributes{file: f}
}

// GetVersionNeededToExtract returns minimum ZIP version needed
func (fa *fileAttributes) GetVersionNeededToExtract() uint16 {
	if fa.file.config.CompressionMethod == LZMA {
		return 63
	}
	if fa.file.config.CompressionMethod == BZIP2 {
		return 46
	}
	if fa.file.RequiresZip64() {
		return 45
	}
	if fa.file.config.CompressionMethod == Deflate64 {
		return 21
	}
	if fa.file.config.CompressionMethod == Deflated {
		return 20
	}
	if fa.file.isDir || fa.file.path != "" {
		return 20
	}
	if fa.file.config.IsEncrypted {
		return 20
	}
	return 10
}

// GetVersionMadeBy returns version made by field
func (fa *fileAttributes) GetVersionMadeBy() uint16 {
	fs := fa.file.hostSystem
	if fs == HostSystemNTFS {
		fs = HostSystemFAT
	}
	return uint16(fs)<<8 | __LATEST_ZIP_VERSION
}

// GetFileBitFlag returns general purpose bit flag
func (fa *fileAttributes) GetFileBitFlag() uint16 {
	var flag uint16

	if fa.file.config.IsEncrypted {
		flag |= 0x0001
	}

	if fa.file.config.CompressionMethod == Deflated {
		flag |= fa.getCompressionLevelBits()
	}

	return flag
}

// GetExternalFileAttributes returns external file attributes
func (fa *fileAttributes) GetExternalFileAttributes() uint32 {
	if fa.file.hostSystem == HostSystemFAT || fa.file.hostSystem == HostSystemNTFS {
		if fa.file.isDir {
			return 0x10 // Directory attribute
		}
		return 0x20 // Archive attribute
	}
	return 0
}

// getCompressionLevelBits returns compression level bits for DEFLATE compression
func (fa *fileAttributes) getCompressionLevelBits() uint16 {
	level := fa.file.config.CompressionLevel
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

// zipHeaders handles ZIP format header creation
type zipHeaders struct {
	file  *file
	attrs *fileAttributes
}

// newZipHeaders creates a new ZipHeaders instance
func newZipHeaders(f *file) *zipHeaders {
	return &zipHeaders{file: f, attrs: NewFileAttributes(f)}
}

// LocalHeader creates a local file header
func (zh *zipHeaders) LocalHeader() localFileHeader {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)

	return localFileHeader{
		VersionNeededToExtract: zh.attrs.GetVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.attrs.GetFileBitFlag(),
		CompressionMethod:      uint16(zh.file.config.CompressionMethod),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  0, // Will be updated later
		CompressedSize:         0, // Will be updated later
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         zh.file.getFilenameLength(),
		ExtraFieldLength:       0, // Will be updated if ZIP64 is needed
	}
}

// CentralDirEntry creates a central directory entry
func (zh *zipHeaders) CentralDirEntry() centralDirectory {
	dosDate, dosTime := timeToMsDos(zh.file.modTime)

	return centralDirectory{
		VersionMadeBy:          zh.attrs.GetVersionMadeBy(),
		VersionNeededToExtract: zh.attrs.GetVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.attrs.GetFileBitFlag(),
		CompressionMethod:      uint16(zh.file.config.CompressionMethod),
		LastModFileTime:        dosTime,
		LastModFileDate:        dosDate,
		CRC32:                  zh.file.crc32,
		CompressedSize:         uint32(min(math.MaxUint32, zh.file.compressedSize)),
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         zh.file.getFilenameLength(),
		ExtraFieldLength:       zh.file.getExtraFieldLength(),
		FileCommentLength:      uint16(len(zh.file.config.Comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: zh.attrs.GetExternalFileAttributes(),
		LocalHeaderOffset:      uint32(min(math.MaxUint32, zh.file.localHeaderOffset)),
	}
}

// FileMetadata handles file metadata and extra fields
type FileMetadata struct {
	file *file
}

// ExtraFieldEntry represents an external file data
type ExtraFieldEntry struct {
	Tag  uint16 // Extra field identifier
	Data []byte // Encoded field data including tag
}

// NewFileMetadata creates a new FileMetadata instance
func NewFileMetadata(f *file) *FileMetadata {
	return &FileMetadata{file: f}
}

// GetExtraField returns extra field entry with given tag
func (fm *FileMetadata) GetExtraField(tag uint16) ExtraFieldEntry {
	for _, entry := range fm.file.extraField {
		if entry.Tag == tag {
			return entry
		}
	}
	return ExtraFieldEntry{}
}

// HasExtraField checks if extra field with given tag exists
func (fm *FileMetadata) HasExtraField(tag uint16) bool {
	for _, entry := range fm.file.extraField {
		if entry.Tag == tag {
			return true
		}
	}
	return false
}

// AddExtraFieldEntry adds an extra field entry
func (fm *FileMetadata) AddExtraFieldEntry(entry ExtraFieldEntry) error {
	if fm.HasExtraField(entry.Tag) {
		return errors.New("entry with the same tag already exists")
	}
	if int(fm.file.getExtraFieldLength())+len(entry.Data) > math.MaxUint16 {
		return errors.New("extra field length limit exceeded")
	}
	fm.file.extraField = append(fm.file.extraField, entry)
	return nil
}

// AddFilesystemExtraField creates and sets extra fields with file metadata
func (fm *FileMetadata) AddFilesystemExtraField() {
	if fm.file.metadata == nil {
		return
	}
	if fm.file.hostSystem == HostSystemNTFS && !fm.HasExtraField(__NTFS_METADATA_FIELD_ID) {
		fm.addNTFSExtraField()
	}
}

// addZip64ExtraField adds ZIP64 extra field for large files
func (fm *FileMetadata) addZip64ExtraField() {
	data := make([]byte, 4, 28)
	binary.LittleEndian.PutUint16(data[0:2], __ZIP64_EXTRA_FIELD_ID)

	var buf [8]byte
	if fm.file.uncompressedSize > math.MaxUint32 {
		binary.LittleEndian.PutUint64(buf[:], uint64(fm.file.uncompressedSize))
		data = append(data, buf[:]...)
	}
	if fm.file.compressedSize > math.MaxUint32 {
		binary.LittleEndian.PutUint64(buf[:], uint64(fm.file.compressedSize))
		data = append(data, buf[:]...)
	}
	if fm.file.localHeaderOffset > math.MaxUint32 {
		binary.LittleEndian.PutUint64(buf[:], uint64(fm.file.localHeaderOffset))
		data = append(data, buf[:]...)
	}

	binary.LittleEndian.PutUint16(data[2:4], uint16(len(data)-4))
	fm.AddExtraFieldEntry(ExtraFieldEntry{Tag: __ZIP64_EXTRA_FIELD_ID, Data: data})
}

// addNTFSExtraField adds NTFS timestamp extra field
func (fm *FileMetadata) addNTFSExtraField() {
	var mtime, atime, ctime uint64
	if val, ok := fm.file.metadata["LastWriteTime"]; ok {
		if t, ok := val.(uint64); ok {
			mtime = t
		}
	}
	if val, ok := fm.file.metadata["LastAccessTime"]; ok {
		if t, ok := val.(uint64); ok {
			atime = t
		}
	}
	if val, ok := fm.file.metadata["CreationTime"]; ok {
		if t, ok := val.(uint64); ok {
			ctime = t
		}
	}

	// Tag(2) + Size(2) + Reserved(4) + Attr1(2) + Size1(2) + Mtime(8) + Atime(8) + Ctime(8)
	data := make([]byte, 36)

	binary.LittleEndian.PutUint16(data[0:2], __NTFS_METADATA_FIELD_ID)
	binary.LittleEndian.PutUint16(data[2:4], 32)   // Block size
	binary.LittleEndian.PutUint32(data[4:8], 0)    // Reserved
	binary.LittleEndian.PutUint16(data[8:10], 1)   // Attribute1 (Tag 1)
	binary.LittleEndian.PutUint16(data[10:12], 24) // Size1 (Size of attributes)
	binary.LittleEndian.PutUint64(data[12:20], mtime)
	binary.LittleEndian.PutUint64(data[20:28], atime)
	binary.LittleEndian.PutUint64(data[28:36], ctime)

	fm.AddExtraFieldEntry(ExtraFieldEntry{Tag: __NTFS_METADATA_FIELD_ID, Data: data})
}
