package gozip

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
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
	localHeaderOffset uint32
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
		hostSystem:       getHostSystem(),
		source:           f,
	}, nil
}

// newFileFromReader creates a File from an io.Reader
func newFileFromReader(source io.Reader, name string) (*file, error) {
	return &file{
		name:       name,
		modTime:    time.Now(),
		hostSystem: getHostSystem(),
		source:     source,
	}, nil
}

// newDirectoryFile creates a File representing a directory
func newDirectoryFile(dirPath, dirName string) (*file, error) {
	return &file{
		name:       dirName,
		path:       dirPath,
		isDir:      true,
		hostSystem: getHostSystem(),
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

func (f *file) SetConfig(config FileConfig) { f.config = config }
func (f *file) SetSource(source io.Reader)  { f.source = source }

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
    return uint16(fa.file.hostSystem)<<8 | __LATEST_ZIP_VERSION
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
    if fa.file.hostSystem == HostSystemFAT {
        if fa.file.isDir {
            return 0x10
        }
        return 0x20
    }
    return 0
}

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
		UncompressedSize:       uint32(zh.file.uncompressedSize),
		FilenameLength:         zh.getFilenameLength(),
		ExtraFieldLength:       zh.getExtraFieldLength(),
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
		CompressedSize:         uint32(zh.file.compressedSize),
		UncompressedSize:       uint32(zh.file.uncompressedSize),
		FilenameLength:         zh.getFilenameLength(),
		ExtraFieldLength:       zh.file.getExtraFieldLength(),
		FileCommentLength:      uint16(len(zh.file.config.Comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: zh.attrs.GetExternalFileAttributes(),
		LocalHeaderOffset:      zh.file.localHeaderOffset,
	}
}

func (zh *zipHeaders) getFilenameLength() uint16 {
	filenameLength := uint16(len(path.Join(zh.file.path, zh.file.name)))
	if zh.file.isDir {
		filenameLength++
	}
	return filenameLength
}

func (zh *zipHeaders) getExtraFieldLength() uint16 {
	if zh.file.HasExtraField(__ZIP64_EXTRA_FIELD) {
		return 32
	}
	return 0
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

// UpdateFileMetadataExtraField updates extra fields with file metadata
func (fm *FileMetadata) UpdateFileMetadataExtraField() {
	if fm.file.metadata == nil {
		return
	}

	if fm.file.hostSystem == HostSystemNTFS && !fm.file.HasExtraField(__NTFS_METADATA_FIELD) {
		fm.addNTFSExtraField()
	}
}

// AddExtraFieldEntry adds an extra field entry
func (fm *FileMetadata) AddExtraFieldEntry(entry ExtraFieldEntry) error {
	if fm.file.HasExtraField(entry.Tag) {
		return errors.New("entry with the same tag already exists")
	}
	if int(fm.file.getExtraFieldLength())+len(entry.Data) > math.MaxUint16 {
		return errors.New("extra field length limit exceeded")
	}
	fm.file.extraField = append(fm.file.extraField, entry)
	return nil
}

// HasExtraField checks if extra field with given tag exists
func (f *file) HasExtraField(tag uint16) bool {
	for _, entry := range f.extraField {
		if entry.Tag == tag {
			return true
		}
	}
	return false
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

func (f *file) getExtraFieldLength() uint16 {
	var size uint16
	for _, entry := range f.extraField {
		size += uint16(len(entry.Data))
	}
	return size
}

// FileOperations handles file compression and writing operations
type FileOperations struct {
	file *file
}

// NewFileOperations creates a new FileOperations instance
func NewFileOperations(f *file) *FileOperations {
	return &FileOperations{file: f}
}

// CompressAndWrite compresses the file content and writes it to destination
func (fo *FileOperations) CompressAndWrite(dest io.Writer) error {
	var uncompressedSize int64
	var err error

	hasher := crc32.NewIEEE()
	sizeCounter := &byteCounterWriter{dest: dest}

	switch fo.file.config.CompressionMethod {
	case Stored:
		uncompressedSize, err = fo.writeStored(sizeCounter, hasher)
	case Deflated:
		uncompressedSize, err = fo.writeDeflated(sizeCounter, hasher)
	default:
		return errors.New("unsupported compression method")
	}

	if err != nil {
		return err
	}

	fo.file.uncompressedSize = uncompressedSize
	fo.file.compressedSize = sizeCounter.bytesWritten
	fo.file.crc32 = hasher.Sum32()
	return nil
}

// UpdateLocalHeader updates the local file header with actual values
func (fo *FileOperations) UpdateLocalHeader(dest *os.File, offset int64) error {
	currentPos, err := dest.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	defer dest.Seek(currentPos, io.SeekStart)

	_, err = dest.Seek(offset+14, io.SeekStart)
	if err != nil {
		return err
	}

	binary.Write(dest, binary.LittleEndian, fo.file.crc32)
	binary.Write(dest, binary.LittleEndian, uint32(fo.file.compressedSize))
	binary.Write(dest, binary.LittleEndian, uint32(fo.file.uncompressedSize))

	return nil
}

func (fo *FileOperations) writeStored(counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	multiWriter := io.MultiWriter(counter, hasher)
	return io.Copy(multiWriter, fo.file.source)
}

func (fo *FileOperations) writeDeflated(counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	tee := io.TeeReader(fo.file.source, hasher)

	level := fo.file.config.CompressionLevel
	if level == 0 {
		level = DeflateNormal
	}

	compressor, err := flate.NewWriter(counter, level)
	if err != nil {
		return 0, err
	}
	defer compressor.Close()

	uncompressedSize, err := io.Copy(compressor, tee)
	if err != nil {
		return 0, err
	}

	return uncompressedSize, compressor.Close()
}