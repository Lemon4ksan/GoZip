package gozip

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path"
	"runtime"
	"syscall"
	"time"
)

const __LATEST_ZIP_VERSION uint16 = 63

const (
	__ZIP64_EXTRA_FIELD uint16 = 0x001
	__NTFS_METADATA_FIELD uint16 = 0x00A
)

// file represents a file to be compressed and added to a ZIP archive.
// It contains file metadata, compression information, and the file content source.
type file struct {
	// Basic file identification
	name  string // File name
	path  string // Path in zip archive
	isDir bool   // Whether file is a directory

	// File content and source
	source           io.Reader // Source of file data to be compressed
	uncompressedSize int64     // Original file size
	compressedSize   int64     // Size after compression
	crc32            uint32    // CRC32 checksum of uncompressed data

	// Compression settings
	compressionMethod CompressionMethod // Compression algorithm used
	compressionLevel  int               // Algorithm dependent compression level

	// ZIP archive structure
	localHeaderOffset uint32     // Offset of local file header in archive
	hostSystem        HostSystem // File host system for attribute compatibility

	// Metadata and timestamps
	modTime    time.Time              // File modification time
	metadata   map[string]interface{} // System specific metadata (NTFS, UNIX, etc.)
	extraField []ExtraFieldEntry      // Extra data that will be added to zip archive

	// Additional information
	comment     string // Optional file comment
	isEncrypted bool   // Whether file is encrypted
}

// newFileFromOS creates a file struct from an [os.File] instance.
// It extracts basic file information like name, size, and modification time.
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

// newFileFromReader creates a file struct from an [io.Reader] instance.
// File's modification time will be set to current system time.
func newFileFromReader(source io.Reader, name string) (*file, error) {
	return &file{
		name:       name,
		modTime:    time.Now(),
		hostSystem: getHostSystem(),
		source:     source,
	}, nil
}

// newDirectoryFile creates a file struct representing a directory in the ZIP archive
func newDirectoryFile(dirPath, dirName string) (*file, error) {
	return &file{
		name:       dirName,
		path:       dirPath,
		isDir:      true,
		hostSystem: getHostSystem(),
		modTime:    time.Now(),
	}, nil
}

// compressAndWrite compresses the file content and writes it to the destination.
// It calculates CRC32 checksum during writing and handles different compression methods.
func (f *file) compressAndWrite(dest io.Writer) error {
	var uncompressedSize int64
	var err error
	hasher := crc32.NewIEEE()
	sizeCounter := &byteCounterWriter{dest: dest}

	switch f.compressionMethod {
	case Stored:
		multiWriter := io.MultiWriter(sizeCounter, hasher)
		uncompressedSize, err = io.Copy(multiWriter, f.source)
		if err != nil {
			return err
		}
	case Deflated:
		tee := io.TeeReader(f.source, hasher)

		if f.compressionLevel == 0 {
			f.compressionLevel = DeflateNormal
		}
		compressor, err := flate.NewWriter(sizeCounter, f.compressionLevel)
		if err != nil {
			compressor.Close()
			return err
		}

		uncompressedSize, err = io.Copy(compressor, tee)
		if err != nil {
			return err
		}

		if err = compressor.Close(); err != nil {
			return err
		}
	}

	f.uncompressedSize = uncompressedSize
	f.compressedSize = sizeCounter.bytesWritten
	f.crc32 = hasher.Sum32()
	return nil
}

// updateLocalHeader updates the local file header with actual CRC32 and compressed size.
// This should be called after compression when the actual values are known.
func (f *file) updateLocalHeader(dest *os.File, offset int64) error {
	currentPos, err := dest.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	defer dest.Seek(currentPos, io.SeekStart)

	_, err = dest.Seek(offset+14, io.SeekStart)
	if err != nil {
		return err
	}

	binary.Write(dest, binary.LittleEndian, f.crc32)
	binary.Write(dest, binary.LittleEndian, uint32(f.compressedSize))
	binary.Write(dest, binary.LittleEndian, uint32(f.uncompressedSize))

	return nil
}

// localHeader creates a local file header structure for ZIP format.
// Local file headers appear before each file's compressed data in the archive.
// The header contains metadata about the file and compression information.
func (f *file) localHeader() localFileHeader {
	d, t := timeToMsDos(f.modTime)
	filenameLength := uint16(len(path.Join(f.path, f.name)))
	if f.isDir {
		filenameLength++ // Directories have extra / at the end
	}
	var extraFieldLength uint16
	if f.HasExtraField(__ZIP64_EXTRA_FIELD) {
		extraFieldLength = 32
	}
	return localFileHeader{
		VersionNeededToExtract: f.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  f.getFileBitFlag(),
		CompressionMethod:      uint16(f.compressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
		CRC32:                  0, // Will  be  updated
		CompressedSize:         0, // after compression
		UncompressedSize:       uint32(f.uncompressedSize),
		FilenameLength:         filenameLength,
		ExtraFieldLength:       extraFieldLength,
	}
}

// centralDirEntry creates a central directory entry for ZIP format.
// Central directory contains metadata about all files in the archive and is located at the end.
// This entry is used for quick file lookup without scanning the entire archive.
func (f *file) centralDirEntry() centralDirectory {
	d, t := timeToMsDos(f.modTime)
	filenameLength := uint16(len(path.Join(f.path, f.name)))
	if f.isDir {
		filenameLength++ // Directories have extra / at the end
	}
	return centralDirectory{
		VersionMadeBy:          f.getVersionMadeBy(),
		VersionNeededToExtract: f.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  f.getFileBitFlag(),
		CompressionMethod:      uint16(f.compressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
		CRC32:                  f.crc32,
		CompressedSize:         uint32(f.compressedSize),
		UncompressedSize:       uint32(f.uncompressedSize),
		FilenameLength:         filenameLength,
		ExtraFieldLength:       f.getExtraFieldLength(),
		FileCommentLength:      uint16(len(f.comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: f.getExternalFileAttributes(),
		LocalHeaderOffset:      f.localHeaderOffset,
	}
}

// getVersionNeededToExtract returns minimum supported ZIP specification version needed to extract the file
func (f *file) getVersionNeededToExtract() uint16 {
	var version uint16 = 10

	if f.isDir || f.path != "" {
		version = 20
	} else if f.compressionMethod == Deflated {
		version = 20
	}
	return version
}

// getVersionMadeBy returns the version made by field for ZIP central directory
func (f *file) getVersionMadeBy() uint16 {
	return uint16(f.hostSystem)<<8 | __LATEST_ZIP_VERSION
}

// getFileBitFlag returns bit flag for the file according to ZIP specification
func (f *file) getFileBitFlag() uint16 {
	var flag uint16

	// Bit 0: encryption
	if f.isEncrypted {
		flag |= 0x0001
	}

	// Bits 1-2: compression level (for Deflate only)
	if f.compressionMethod == Deflated {
		var levelBits uint16

		if f.compressionLevel == 0 {
			f.compressionLevel = DeflateNormal
		}

		switch f.compressionLevel {
		case DeflateSuperFast: // Super Fast
			levelBits = 0x0006 // bits 1 and 2: 1,1
		case DeflateFast: // Fast
			levelBits = 0x0004 // bits 1 and 2: 1,0
		case DeflateMaximum: // Maximum
			levelBits = 0x0002 // bits 1 and 2: 0,1
		case DeflateNormal: // Normal (default)
			fallthrough
		default:
			levelBits = 0x0000 // bits 1 and 2: 0,0
		}
		flag |= levelBits
	}

	return flag
}

func (f *file) updateFileMetadataExtraField() {
	if f.metadata == nil {
		return
	}
	var tag uint16
	buf := new(bytes.Buffer)

	if f.hostSystem == HostSystemNTFS && !f.HasExtraField(__NTFS_METADATA_FIELD){
		tag = __NTFS_METADATA_FIELD
		var mtime, atime, ctime uint64
		if mTime, ok := f.metadata["LastWriteTime"].(uint64); ok {
			mtime = mTime
		}
		if aTime, ok := f.metadata["LastAccessTime"].(uint64); ok {
			atime = aTime
		}
		if cTime, ok := f.metadata["CreationTime"].(uint64); ok {
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
			tag, 32, 0, 1, 24, mtime, atime, ctime,
		}
		binary.Write(buf, binary.LittleEndian, ntfs)
	}

	if tag != 0 {
		f.extraField = append(f.extraField, ExtraFieldEntry{tag, buf.Bytes()})
	}
}

func (f *file) getExternalFileAttributes() uint32 {
	if f.hostSystem == HostSystemNTFS {
		if f.isDir {
			return 0x10
		} else {
			return 0x20
		}
	}
	return 0
}

func (f *file) getExtraFieldLength() uint16 {
	var size uint16
	for _, entry := range f.extraField {
		size += uint16(len(entry.Data))
	}
	return size
}

func (f *file) HasExtraField(tag uint16) bool {
	for _, entry := range f.extraField {
		if entry.Tag == tag {
			return true
		}
	}
	return false
}

func (f *file) AddExtraFieldEntry(entry ExtraFieldEntry) error {
	if f.HasExtraField(entry.Tag) {
		return errors.New("entry with the same tag already exists")
	}
	if int(f.getExtraFieldLength()) + len(entry.Data) > math.MaxUint16 {
		return errors.New("extra field length limit exceeded")
	}
	f.extraField = append(f.extraField, entry)
	return nil
}

// byteCounterWriter is a wrapper around [io.Writer] that counts the total number of bytes written.
// It's used to track compressed size during the compression process.
type byteCounterWriter struct {
	dest         io.Writer
	bytesWritten int64
}

// Write implements the io.Writer interface and counts bytes written.
// It delegates the actual writing to the underlying writer and accumulates the byte count.
func (w *byteCounterWriter) Write(p []byte) (int, error) {
	n, err := w.dest.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

// timeToMsDos converts time.Time to MS-DOS date and time format.
// MS-DOS date format: bits 0-4: day (1-31), bits 5-8: month (1-12), bits 9-15: years from 1980 (0-127).
// MS-DOS time format: bits 0-4: seconds/2 (0-29), bits 5-10: minutes (0-59), bits 11-15: hours (0-23).
// Note: Seconds are stored divided by 2, limiting resolution to 2-second intervals.
// Years before 1980 are clamped to 1980, years after 2107 are clamped to 2107.
func timeToMsDos(t time.Time) (date uint16, time uint16) {
	year := max(t.Year()-1980, 0)
	if year > 127 {
		year = 127
	}

	month := uint16(t.Month())
	day := uint16(t.Day())
	hour := uint16(t.Hour())
	minute := uint16(t.Minute())
	second := uint16(t.Second())

	// MS-DOS date format: YYYYYYYM MMMDDDDD
	date = uint16(year)<<9 | uint16(month)<<5 | day

	// MS-DOS time format: HHHHHMMM MMMSSSSS (seconds divided by 2)
	time = uint16(hour)<<11 | uint16(minute)<<5 | uint16(second/2)

	return date, time
}

// msDosToTime converts MS-DOS date and time format to [time.Time].
// Handles the reverse conversion of TimeToMsDos, reconstructing the original time.
// Note: Time resolution is limited to 2 seconds due to the original format constraints.
// The returned time is always in UTC timezone.
func msDosToTime(dosDate uint16, dosTime uint16) time.Time {
	day := dosDate & 0x1F                 // bits 0-4: day
	month := (dosDate >> 5) & 0x0F        // bits 5-8: month
	year := int((dosDate>>9)&0x7F) + 1980 // bits 9-15: years from 1980
	second := (dosTime & 0x1F) * 2        // bits 0-4: seconds/2 (multiply by 2 to restore)
	minute := (dosTime >> 5) & 0x3F       // bits 5-10: minutes
	hour := (dosTime >> 11) & 0x1F        // bits 11-15: hours

	return time.Date(
		year,
		time.Month(month),
		int(day),
		int(hour),
		int(minute),
		int(second),
		0,
		time.UTC,
	)
}

func getFileMetadata(stat os.FileInfo) map[string]interface{} {
	metadata := make(map[string]interface{})
	if s, ok := stat.Sys().(*syscall.Win32FileAttributeData); ok {
		metadata["LastWriteTime"] = uint64(s.LastWriteTime.Nanoseconds())/100 + 116444736000000000
		metadata["LastAccessTime"] = uint64(s.LastAccessTime.Nanoseconds())/100 + 116444736000000000
		metadata["CreationTime"] = uint64(s.CreationTime.Nanoseconds())/100 + 116444736000000000
		metadata["FileAttributes"] = s.FileAttributes
	}

	return metadata
}

// getHostSystem determines the appropriate host system based on the current
// operating system using runtime.GOOS. This helps extraction tools understand
// how to interpret external file attributes.
func getHostSystem() HostSystem {
	switch runtime.GOOS {
	case "windows":
		return HostSystemNTFS
	case "darwin":
		return HostSystemDarwin
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return HostSystemUNIX
	case "android":
		return HostSystemUNIX
	case "aix":
		return HostSystemUNIX
	case "solaris", "illumos":
		return HostSystemUNIX
	case "plan9":
		return HostSystemUNIX
	case "zos":
		return HostSystemMVS
	default:
		// For unknown systems, use FAT for maximum compatibility
		return HostSystemFAT
	}
}
