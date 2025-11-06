package gozip

import (
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"time"
)

const __LATEST_ZIP_VERSION uint16 = 63

// file represents a file to be compressed and added to a ZIP archive.
// It contains file metadata, compression information, and the file content source.
type file struct {
	name              string
	crc32             uint32            // CRC32 checksum of uncompressed data
	modTime           time.Time         // File modification time
	compressedSize    int64             // Size after compression
	uncompressedSize  int64             // Original file size
	compressionMethod CompressionMethod // Compression algorithm used
	compressionLevel  int        		// Algorithm depended compression level
	localHeaderOffset uint32            // Offset of local file header in archive
	comment           string            // Optional file comment
	isEncrypted	      bool				// Whether file is encrypted
	source            io.Reader         // Source of file data to be compressed
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
		source:           f,
	}, nil
}

// newFileFromOS creates a file struct from an [io.Reader] instance.
// File's modification time will be set to current system time.
func newFileFromReader(source io.Reader, name string) (*file, error) {
	return &file{
		name:    name,
		modTime: time.Now(),
		source:  source,
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
// Local file headers appear before each file's compressed data.
func (f *file) localHeader() localFileHeader {
	d, t := timeToMsDos(f.modTime)
	return localFileHeader{
		VersionNeededToExtract: f.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  f.getFileBitFlag(),
		CompressionMethod:      uint16(f.compressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
		CRC32:                  0, // Will  be  updated
		CompressedSize:         0, // after compression
		UncompressedSize:       uint32(f.uncompressedSize),
		FilenameLength:         uint16(len(f.name)),
		ExtraFieldLength:       0,
	}
}

// centralDirEntry creates a central directory entry for ZIP format.
// Central directory contains metadata about all files in the archive.
func (f *file) centralDirEntry() centralDirectory {
	d, t := timeToMsDos(f.modTime)
	return centralDirectory{
		VersionMadeBy:          __LATEST_ZIP_VERSION,
		VersionNeededToExtract: f.getVersionNeededToExtract(),
		GeneralPurposeBitFlag:  f.getFileBitFlag(),
		CompressionMethod:      uint16(f.compressionMethod),
		LastModFileTime:        t,
		LastModFileDate:        d,
		CRC32:                  f.crc32,
		CompressedSize:         uint32(f.compressedSize),
		UncompressedSize:       uint32(f.uncompressedSize),
		FilenameLength:         uint16(len(f.name)),
		ExtraFieldLength:       0,
		FileCommentLength:      uint16(len(f.comment)),
		DiskNumberStart:        0,
		InternalFileAttributes: 0,
		ExternalFileAttributes: 0,
		LocalHeaderOffset:      f.localHeaderOffset,
	}
}

// getVersionNeededToExtract returns minimum supported
// ZIP specification version needed to extract the file
func (f *file) getVersionNeededToExtract() uint16 {
	var version uint16 = 10

	if f.compressionMethod == Deflated {
		version = 20
	}
	return version
}

// getFileBitFlag returns bit flag for the file
func (f *file) getFileBitFlag() uint16 {
    flag := uint16(0)

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
        case DeflateFast:      // Fast
            levelBits = 0x0004 // bits 1 and 2: 1,0  
        case DeflateMaximum:   // Maximum
            levelBits = 0x0002 // bits 1 and 2: 0,1
        case DeflateNormal:    // Normal (default)
            fallthrough
        default:
            levelBits = 0x0000 // bits 1 and 2: 0,0
        }
        flag |= levelBits
    }
    
    return flag
}

type byteCounterWriter struct {
	dest         io.Writer
	bytesWritten int64
}

func (w *byteCounterWriter) Write(p []byte) (int, error) {
	n, err := w.dest.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

// timeToMsDos converts time.Time to MS-DOS date and time format.
// MS-DOS date format: bits 0-4: day (1-31), bits 5-8: month (1-12), bits 9-15: years from 1980 (0-127).
// MS-DOS time format: bits 0-4: seconds/2 (0-29), bits 5-10: minutes (0-59), bits 11-15: hours (0-23).
// Note: Seconds are stored divided by 2, limiting resolution to 2-second intervals.
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
