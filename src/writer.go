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

// WriteFile processes and writes a single file to the archive
func (zw *zipWriter) WriteFile(file *file) error {
	meta := NewFileMetadata(file)
	meta.AddFilesystemExtraField()

	if err := zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write file header: %w", err)
	}

	if !file.isDir {
		tmpFile, err := zw.encodeFileData(file)
		if err != nil {
			return fmt.Errorf("encode file data: %w", err)
		}
		defer cleanupTempFile(tmpFile)

		if err := zw.updateLocalHeader(file); err != nil {
			return fmt.Errorf("update local header: %w", err)
		}

		if err := zw.writeFileData(tmpFile); err != nil {
			return fmt.Errorf("write file data: %w", err)
		}
	}

	if file.RequiresZip64() {
		meta.addZip64ExtraField()
	}

	return zw.addCentralDirEntry(file)
}

// writeCentralDirectory writes the central directory and end records
func (zw *zipWriter) WriteCentralDirAndEndRecords() error {
	if _, err := zw.dest.Write(zw.centralDirBuf.Bytes()); err != nil {
		return fmt.Errorf("write central directory: %w", err)
	}

	if zw.sizeOfCentralDir > math.MaxUint32 || zw.localHeaderOffset > math.MaxUint32 {
		if err := zw.writeZip64EndHeaders(); err != nil {
			return fmt.Errorf("write zip64 headers: %w", err)
		}
	}

	endOfCentralDir := encodeEndOfCentralDirRecord(
		zw.zip,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.localHeaderOffset),
	)
	if _, err := zw.dest.Write(endOfCentralDir); err != nil {
		return fmt.Errorf("write end of central directory: %w", err)
	}
	return nil
}

// writeFileHeader writes the local file header for a file entry
func (zw *zipWriter) writeFileHeader(file *file) error {
	file.localHeaderOffset = zw.localHeaderOffset
	header := newZipHeaders(file).LocalHeader()
	n, err := zw.dest.Write(header.encode(file))
	if err != nil {
		return fmt.Errorf("write local header: %w", err)
	}
	zw.localHeaderOffset += int64(n)
	return nil
}

// addCentralDirEntry adds a central directory entry for a file
func (zw *zipWriter) addCentralDirEntry(file *file) error {
	cdData := newZipHeaders(file).CentralDirEntry()
	n, err := zw.centralDirBuf.Write(cdData.encode(file))
	if err != nil {
		return fmt.Errorf("write central directory entry: %w", err)
	}
	zw.sizeOfCentralDir += int64(n)
	return nil
}

// encodeFileData compresses file data according to file config
func (zw *zipWriter) encodeFileData(file *file) (*os.File, error) {
	var uncompressedSize int64
	var tmpFile *os.File
	var err error

	// Use temporary file for large files that might need ZIP64
	sizeCounter := &byteCounterWriter{dest: zw.dest}
	if file.uncompressedSize > math.MaxUint32 {
		tmpFile, err = os.CreateTemp("", "zip-compress-*")
		if err != nil {
			return nil, fmt.Errorf("create temp file: %w", err)
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
		cleanupTempFile(tmpFile)
		return nil, fmt.Errorf("compression: %w", err)
	}

	file.uncompressedSize = uncompressedSize
	file.compressedSize = sizeCounter.bytesWritten
	file.crc32 = hasher.Sum32()
	zw.localHeaderOffset += file.compressedSize

	return tmpFile, nil
}

// writeFileData copies data from temporary file to final destination
func (zw *zipWriter) writeFileData(tmpFile *os.File) error {
	if tmpFile == nil {
		return nil
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file: %w", err)
	}
	if _, err := io.Copy(zw.dest, tmpFile); err != nil {
		return fmt.Errorf("copy temp file data: %w", err)
	}
	return nil
}

// updateLocalHeader updates the local file header with actual compression results
func (zw *zipWriter) updateLocalHeader(file *file) error {
	// Seek to CRC position in local header
	if _, err := zw.dest.Seek(file.localHeaderOffset+14, io.SeekStart); err != nil {
		return fmt.Errorf("seek to CRC position: %w", err)
	}

	if err := binary.Write(zw.dest, binary.LittleEndian, file.crc32); err != nil {
		return fmt.Errorf("write CRC: %w", err)
	}
	if err := binary.Write(zw.dest, binary.LittleEndian, uint32(min(math.MaxUint32, file.compressedSize))); err != nil {
		return fmt.Errorf("write compressed size: %w", err)
	}
	if err := binary.Write(zw.dest, binary.LittleEndian, uint32(min(math.MaxUint32, file.uncompressedSize))); err != nil {
		return fmt.Errorf("write uncompressed size: %w", err)
	}

	if file.RequiresZip64() {
		return zw.writeZip64ExtraField(file)
	} else {
		if _, err := zw.dest.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("seek to end of the file: %w", err)
		}
	}
	return nil
}

// writeZip64ExtraField writes ZIP64 extra field for large files
func (zw *zipWriter) writeZip64ExtraField(file *file) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __ZIP64_EXTRA_FIELD_ID)
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

	// Update extra field length
	if _, err := zw.dest.Seek(2, io.SeekCurrent); err != nil {
		return fmt.Errorf("seek to extra field length: %w", err)
	}
	if err := binary.Write(zw.dest, binary.LittleEndian, size+4); err != nil {
		return fmt.Errorf("write extra field length: %w", err)
	}

	// Skip filename and write extra field
	if _, err := zw.dest.Seek(int64(file.getFilenameLength()), io.SeekCurrent); err != nil {
		return fmt.Errorf("seek past filename: %w", err)
	}
	if _, err := zw.dest.Write(extraField); err != nil {
		return fmt.Errorf("write extra field: %w", err)
	}

	zw.localHeaderOffset += int64(size + 4)
	return nil
}

// writeStored implements the "store" compression method (no compression)
func writeStored(file *file, counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	multiWriter := io.MultiWriter(counter, hasher)
	size, err := io.Copy(multiWriter, file.source)
	if err != nil {
		return 0, fmt.Errorf("copy data: %w", err)
	}
	return size, nil
}

// writeDeflated implements the DEFLATE compression method
func writeDeflated(file *file, counter *byteCounterWriter, hasher hash.Hash32) (int64, error) {
	tee := io.TeeReader(file.source, hasher)

	level := file.config.CompressionLevel
	if level == 0 {
		level = DeflateNormal
	}

	compressor, err := flate.NewWriter(counter, level)
	if err != nil {
		return 0, fmt.Errorf("create flate writer: %w", err)
	}
	defer compressor.Close()

	uncompressedSize, err := io.Copy(compressor, tee)
	if err != nil {
		return 0, fmt.Errorf("compress data: %w", err)
	}

	if err := compressor.Close(); err != nil {
		return 0, fmt.Errorf("close compressor: %w", err)
	}
	return uncompressedSize, nil
}

// writeZip64EndHeaders writes ZIP64 end of central directory record and locator
func (zw *zipWriter) writeZip64EndHeaders() error {
	zip64EndOfCentralDir := encodeZip64EndOfCentralDirectoryRecord(
		zw.zip,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.localHeaderOffset),
	)
	if _, err := zw.dest.Write(zip64EndOfCentralDir); err != nil {
		return fmt.Errorf("write zip64 end of central directory: %w", err)
	}

	zip64EndOfCentralDirLocator := encodeZip64EndOfCentralDirectoryLocator(
		uint64(zw.localHeaderOffset + zw.sizeOfCentralDir),
	)
	if _, err := zw.dest.Write(zip64EndOfCentralDirLocator); err != nil {
		return fmt.Errorf("write zip64 end of central directory locator: %w", err)
	}
	return nil
}

// cleanupTempFile safely cleans up a temporary file
func cleanupTempFile(tmpFile *os.File) {
	if tmpFile != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}
}
