package gozip

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"
)

// zipWriter handles the low-level writing of ZIP archive structure
type zipWriter struct {
	zip               *Zip           // Reference to the parent Zip object
	dest              io.WriteSeeker // Target stream for writing archive data
	sizeOfCentralDir  int64          // Total size of central directory in bytes
	localHeaderOffset int64          // Current offset for local file headers
	centralDirBuf     *bytes.Buffer  // Buffer for accumulating central directory entries
}

// newZipWriter creates and initializes a new zipWriter instance
func newZipWriter(zip *Zip, dest io.WriteSeeker) *zipWriter {
	return &zipWriter{
		zip:           zip,
		dest:          dest,
		centralDirBuf: new(bytes.Buffer),
	}
}

// WriteFileHeader writes the local file header for a file entry.
// Updates the localHeaderOffset with the number of bytes written.
func (zw *zipWriter) WriteFileHeader(file *file) error {
	file.localHeaderOffset = zw.localHeaderOffset
	header := newZipHeaders(file).LocalHeader()
	n, err := zw.dest.Write(header.encode(file))
	if err != nil {
		return err
	}
	zw.localHeaderOffset += int64(n)
	return nil
}

// AddCentralDirEntry adds a central directory entry for a file.
// Accumulates entries in a buffer for later writing.
func (zw *zipWriter) AddCentralDirEntry(file *file) error {
	cdData := newZipHeaders(file).CentralDirEntry()
	n, err := zw.centralDirBuf.Write(cdData.encode(file))
	if err != nil {
		return err
	}
	zw.sizeOfCentralDir += int64(n)
	return nil
}

// EncodeFileData compresses file data according to file config.
// Returns a temporary file handle if file size exceeds uint32 limits.
// Calculates CRC32 and sizes during encoding.
func (zw *zipWriter) EncodeFileData(file *file) (*os.File, error) {
	var uncompressedSize int64
	var tmpFile *os.File
	var err error
	// The length of the Zip64 extension field is unknown,
	// so saving the compressed data must be delayed.
	// Otherwise, it can be copied directly to dest.
	sizeCounter := &byteCounterWriter{dest: zw.dest}
	if file.uncompressedSize > math.MaxUint32 {
		tmpFile, err = os.CreateTemp("", "zip-compress-*")
		if err != nil {
			return nil, err
		}
		sizeCounter.dest = tmpFile
	}

	hasher := crc32.NewIEEE()
	switch file.config.CompressionMethod {
	case Stored:
		uncompressedSize, err = writeStored(file, sizeCounter, hasher)
	case Deflated:
		uncompressedSize, err = writeDeflated(file, sizeCounter, hasher)
	default:
		return nil, errors.New("unsupported compression method")
	}
	if err != nil {
		return nil, err
	}

	file.uncompressedSize = uncompressedSize
	file.compressedSize = sizeCounter.bytesWritten
	file.crc32 = hasher.Sum32()
	zw.localHeaderOffset += file.compressedSize
	return tmpFile, nil
}

// WriteFileData copies data from temporary file to final destination.
// Used for large files that required intermediate storage.
// Returns nil if temporary file is nil.
func (zw *zipWriter) WriteFileData(tmpFile *os.File) error {
	if tmpFile == nil {
		return nil
	}
	tmpFile.Seek(0, io.SeekStart)
	_, err := io.Copy(zw.dest, tmpFile)
	if err != nil {
		return err
	}
	return nil
}

// UpdateLocalHeader updates the local file header with actual compression results.
// Writes CRC32, compressed size, and uncompressed size after they are known.
// Handles ZIP64 extra fields for large files.
func (zw *zipWriter) UpdateLocalHeader(file *file) error {
	currentPos, err := zw.dest.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	_, err = zw.dest.Seek(file.localHeaderOffset+14, io.SeekStart)
	if err != nil {
		return err
	}
	binary.Write(zw.dest, binary.LittleEndian, file.crc32)
	binary.Write(zw.dest, binary.LittleEndian, uint32(min(math.MaxUint32, file.compressedSize)))
	binary.Write(zw.dest, binary.LittleEndian, uint32(min(math.MaxUint32, file.uncompressedSize)))

	if file.RequiresZip64() {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.LittleEndian, __ZIP64_EXTRA_FIELD)
		binary.Write(buf, binary.LittleEndian, uint16(0))

		var size uint16
		if file.uncompressedSize > math.MaxUint32 {
			binary.Write(buf, binary.LittleEndian, file.uncompressedSize)
			size += 8
		}
		if file.compressedSize > math.MaxUint32 {
			binary.Write(buf, binary.LittleEndian, file.compressedSize)
			size += 8
		}
		extraField := buf.Bytes()
		binary.LittleEndian.PutUint16(extraField[2:4], size)

		_, err := zw.dest.Seek(2, io.SeekCurrent)
		if err != nil {
			return err
		}
		binary.Write(zw.dest, binary.LittleEndian, size+4)
		_, err = zw.dest.Seek(int64(file.getFilenameLength()), io.SeekCurrent)
		if err != nil {
			return err
		}
		zw.dest.Write(extraField)
		zw.localHeaderOffset += int64(size + 4)
	} else {
		zw.dest.Seek(currentPos, io.SeekStart)
	}
	return nil
}

// writeStored implements the "store" compression method (no compression).
// Writes file data directly while calculating CRC32 and counting bytes.
func writeStored(file *file, counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	multiWriter := io.MultiWriter(counter, hasher)
	return io.Copy(multiWriter, file.source)
}

// writeDeflated implements the DEFLATE compression method.
// Compresses data using flate package while calculating CRC32.
func writeDeflated(file *file, counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	tee := io.TeeReader(file.source, hasher)

	level := file.config.CompressionLevel
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

// WriteCentralDirAndEndDir writes the central directory and end of central directory records.
// Automatically handles ZIP64 format when archive size exceeds traditional limits.
func (zw *zipWriter) WriteCentralDirAndEndDir() error {
	_, err := zw.dest.Write(zw.centralDirBuf.Bytes())
	if err != nil {
		return err
	}
	if zw.sizeOfCentralDir > math.MaxUint32 || zw.localHeaderOffset > math.MaxUint32 {
		err := zw.writeZip64EndHeaders()
		if err != nil {
			return fmt.Errorf("zip64 end headers: %v", err)
		}
	}
	endOfCentralDir := encodeEndOfCentralDirRecord(
		zw.zip,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.localHeaderOffset),
	)
	_, err = zw.dest.Write(endOfCentralDir)
	if err != nil {
		return fmt.Errorf("end of central directory: %v", err)
	}
	return nil
}

// writeZip64EndHeaders writes ZIP64 end of central directory record and locator.
// Required when archive size or file counts exceed traditional ZIP format limits.
func (zw *zipWriter) writeZip64EndHeaders() error {
	zip64EndOfCentralDir := encodeZip64EndOfCentralDirectoryRecord(
		zw.zip,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.localHeaderOffset),
	)
	_, err := zw.dest.Write(zip64EndOfCentralDir)
	if err != nil {
		return err
	}
	zip64EndOfCentralDirLocator := encodeZip64EndOfCentralDirectoryLocator(
		uint64(zw.localHeaderOffset + zw.sizeOfCentralDir),
	)
	_, err = zw.dest.Write(zip64EndOfCentralDirLocator)
	return err
}
