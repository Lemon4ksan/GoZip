package gozip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"time"
)

// file represents a file to be compressed and added to a ZIP archive
type file struct {
	// Basic file identification
	name  string
	path  string
	isDir bool

	// File content and source
	source           io.Reader
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

// newFileFromOS creates a File from an os.File instance
func newFileFromOS(f *os.File) (*file, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	return &file{
		name:             stat.Name(),
		uncompressedSize: stat.Size(),
		modTime:          stat.ModTime(),
		isDir:            stat.IsDir(),
		metadata:         getFileMetadata(stat),
		hostSystem:       getHostSystem(f.Fd()),
		source:           f,
	}, nil
}

// newFileFromReader creates a File from an io.Reader
func newFileFromReader(source io.Reader, name string) (*file, error) {
	return &file{
		name:       name,
		modTime:    time.Now(),
		hostSystem: getHostSystemByOS(),
		source:     source,
	}, nil
}

// newDirectoryFile creates a File representing a directory
func newDirectoryFile(dirPath, dirName string) (*file, error) {
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
	if !f.isDir {
		f.config = config
	} else {
		f.config.Path = config.Path
		f.config.Comment = config.Comment
	}
}
func (f *file) SetSource(source io.Reader)  { f.source = source }

// RequiresZip64 checks whether zip64 extra field should be used for the file
func (f *file) RequiresZip64() bool {
	if f.compressedSize > math.MaxUint32 ||
		f.uncompressedSize > math.MaxUint32 ||
		f.localHeaderOffset > math.MaxUint32 {
		return true
	}
	return false
}

// getExtraFieldLength returns the total length of all extra fields
func (f *file) getExtraFieldLength() uint16 {
	var size uint16
	for _, entry := range f.extraField {
		size += uint16(len(entry.Data))
	}
	return size
}

// getFilenameLength returns the length of the filename in bytes
func (f *file) getFilenameLength() uint16 {
	filenameLength := uint16(len(path.Join(f.path, f.name)))
	if f.isDir {
		filenameLength++
	}
	return filenameLength
}

const __LATEST_ZIP_VERSION uint16 = 63

// fileAttributes handles file attribute calculations
type fileAttributes struct {
	file *file
}

// NewFileAttributes creates a new FileAttributes instance
func NewFileAttributes(f *file) *fileAttributes {
	return &fileAttributes{file: f}
}

// GetVersionNeededToExtract returns minimum ZIP version needed
func (fa *fileAttributes) GetVersionNeededToExtract() uint16 {
	if fa.file.RequiresZip64() {
		return 45
	}
	if fa.file.isDir || fa.file.path != "" {
		return 20
	}
	if fa.file.config.CompressionMethod == Deflated {
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
			return 0x10
		}
		return 0x20
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

const (
	__ZIP64_EXTRA_FIELD   uint16 = 0x0001
	__NTFS_METADATA_FIELD uint16 = 0x000A
)

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
	d, t := timeToMsDos(zh.file.modTime)
	return localFileHeader{
		VersionNeededToExtract: zh.attrs.GetVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.attrs.GetFileBitFlag(),
		CompressionMethod:      uint16(zh.file.config.CompressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
		CRC32:                  0,
		CompressedSize:         0,
		UncompressedSize:       uint32(min(math.MaxUint32, zh.file.uncompressedSize)),
		FilenameLength:         zh.file.getFilenameLength(),
		ExtraFieldLength:       0,
	}
}

// CentralDirEntry creates a central directory entry
func (zh *zipHeaders) CentralDirEntry() centralDirectory {
	d, t := timeToMsDos(zh.file.modTime)

	return centralDirectory{
		VersionMadeBy:          zh.attrs.GetVersionMadeBy(),
		VersionNeededToExtract: zh.attrs.GetVersionNeededToExtract(),
		GeneralPurposeBitFlag:  zh.attrs.GetFileBitFlag(),
		CompressionMethod:      uint16(zh.file.config.CompressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
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
	Tag  uint16
	Data []byte
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

	if fm.file.hostSystem == HostSystemNTFS && !fm.HasExtraField(__NTFS_METADATA_FIELD) {
		fm.addNTFSExtraField()
	}
}

func (fm *FileMetadata) addZip64ExtraField() {
	buf := new(bytes.Buffer)

	binary.Write(buf, binary.LittleEndian, uint16(__ZIP64_EXTRA_FIELD))
	binary.Write(buf, binary.LittleEndian, uint16(0))

	var size uint16
	if fm.file.uncompressedSize > math.MaxUint32 {
		binary.Write(buf, binary.LittleEndian, uint64(fm.file.uncompressedSize))
		size += 8
	}
	if fm.file.compressedSize > math.MaxUint32 {
		binary.Write(buf, binary.LittleEndian, uint64(fm.file.compressedSize))
		size += 8
	}
	if fm.file.localHeaderOffset > math.MaxUint32 {
		binary.Write(buf, binary.LittleEndian, uint64(fm.file.localHeaderOffset))
		size += 8
	}
	data := buf.Bytes()
	binary.LittleEndian.PutUint16(data[2:4], size)
	fm.AddExtraFieldEntry(ExtraFieldEntry{Tag: __ZIP64_EXTRA_FIELD, Data: data})
}

func (fm *FileMetadata) addNTFSExtraField() {
	var mtime, atime, ctime uint64
	if mTime, ok := fm.file.metadata["LastWriteTime"].(uint64); ok {
		mtime = mTime
	}
	if aTime, ok := fm.file.metadata["LastAccessTime"].(uint64); ok {
		atime = aTime
	}
	if cTime, ok := fm.file.metadata["CreationTime"].(uint64); ok {
		ctime = cTime
	}

	ntfs := struct {
		Tag        uint16
		Size       uint16
		Reserved   uint32
		Attribute1 uint16
		Size1      uint16
		Mtime      uint64
		Atime      uint64
		Ctime      uint64
	}{
		__NTFS_METADATA_FIELD, 32, 0, 1, 24, mtime, atime, ctime,
	}

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, ntfs)
	fm.file.extraField = append(fm.file.extraField, ExtraFieldEntry{__NTFS_METADATA_FIELD, buf.Bytes()})
}