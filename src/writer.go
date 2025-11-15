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
	"sync"
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
		err = errors.New("unsupported compression method")
	}
	if err != nil {
		cleanupTempFile(tmpFile)
		return nil, fmt.Errorf("compression: %w", err)
	}

	file.uncompressedSize = uncompressedSize
	file.compressedSize = sizeCounter.bytesWritten
	file.crc32 = hasher.Sum32()
	zw.localHeaderOffset += file.compressedSize

	if tmpFile != nil {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			cleanupTempFile(tmpFile)
			return nil, fmt.Errorf("seek temp file: %w", err)
		}
	}
	return tmpFile, nil
}

// writeFileData copies data from temporary file to final destination
func (zw *zipWriter) writeFileData(tmpFile io.Reader) error {
	if file, ok := tmpFile.(*os.File); ok && file == nil {
        return nil
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

// parallelZipWriter handles parallel compression and writing of files to a ZIP archive
type parallelZipWriter struct {
	zw  *zipWriter
	sem chan struct{}
	memoryThreshold int64
	bufferPool sync.Pool
}

// newParallelZipWriter creates a new parallelZipWriter instance
func newParallelZipWriter(zip *Zip, dest io.WriteSeeker, workers int) *parallelZipWriter {
	return &parallelZipWriter{
		zw:  newZipWriter(zip, dest),
		sem: make(chan struct{}, workers),
		memoryThreshold: 10 * 1024 * 1024, // 10MB default
		bufferPool: sync.Pool{
			New: func() interface{} {
				return NewMemoryBuffer(64 * 1024) // 64KB default
			},
		},
	}
}

// WriteFiles processes multiple files in parallel and writes them to the ZIP archive.
// This method performs compression concurrently using a worker pool but writes
// files sequentially to maintain proper ZIP format structure.
func (pzw *parallelZipWriter) WriteFiles(files []*file) []error {
	var wg sync.WaitGroup
	results := make(chan struct {
		file    *file
		source  io.Reader
		err     error
	}, len(files))

	for _, f := range files {
		wg.Add(1)
		pzw.sem <- struct{}{}

		go func(f *file) {
			defer wg.Done()
			defer func() { <-pzw.sem }()

			source, err := pzw.compressFile(f)
			results <- struct {
				file    *file
				source	io.Reader
				err     error
			}{f, source, err}
		}(f)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []error
	for result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, result.err))
			continue
		}
		if err := pzw.writeCompressedFile(result.file, result.source); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, err))
		}
		if tmpFile, ok := result.source.(*os.File); ok {
			cleanupTempFile(tmpFile)
		}
	}
	return errs
}

// compressFile compresses a single file according to its configuration
func (pzw *parallelZipWriter) compressFile(file *file) (io.Reader, error) {
	if file.isDir {
		return nil, nil
	}

	if file.uncompressedSize > 0 && file.uncompressedSize <= pzw.memoryThreshold {
        return pzw.compressToMemory(file)
    }
    
    return pzw.compressToTempFile(file)
}

// compressToMemory compresses file data to an in-memory buffer instead of temporary file
// Returns an io.ReadWriteSeeker that can be used like a file but operates entirely in memory
func (pzw *parallelZipWriter) compressToMemory(file *file) (io.Reader, error) {
	buffer := pzw.bufferPool.Get().(*MemoryBuffer)

    if int(file.uncompressedSize) > cap(buffer.data) {
        pzw.bufferPool.Put(buffer)
        buffer = NewMemoryBuffer(int(file.uncompressedSize))
    } else {
        buffer.Reset()
    }

    sizeCounter := &byteCounterWriter{dest: buffer}
    hasher := crc32.NewIEEE()

    var uncompressedSize int64
    var err error

    switch file.config.CompressionMethod {
    case Stored:
        uncompressedSize, err = writeStored(file, sizeCounter, hasher)
    case Deflated:
        uncompressedSize, err = writeDeflated(file, sizeCounter, hasher)
    default:
        err = errors.New("unsupported compression method")
    }
    if err != nil {
        pzw.bufferPool.Put(buffer)
        return nil, fmt.Errorf("in-memory compression: %w", err)
    }

    file.uncompressedSize = uncompressedSize
    file.compressedSize = sizeCounter.bytesWritten
    file.crc32 = hasher.Sum32()

    if _, err := buffer.Seek(0, io.SeekStart); err != nil {
        pzw.bufferPool.Put(buffer)
        return nil, fmt.Errorf("reset buffer position: %w", err)
    }
    return buffer, nil
}

func (pzw *parallelZipWriter) compressToTempFile(file *file) (*os.File, error) {
	tmpFile, err := os.CreateTemp("", "zip-compress-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}

	sizeCounter := &byteCounterWriter{dest: tmpFile}
	hasher := crc32.NewIEEE()

	var uncompressedSize int64
	switch file.config.CompressionMethod {
	case Stored:
		uncompressedSize, err = writeStored(file, sizeCounter, hasher)
	case Deflated:
		uncompressedSize, err = writeDeflated(file, sizeCounter, hasher)
	default:
		err = errors.New("unsupported compression method")
	}
	if err != nil {
		cleanupTempFile(tmpFile)
		return nil, fmt.Errorf("compression: %w", err)
	}

	file.uncompressedSize = uncompressedSize
	file.compressedSize = sizeCounter.bytesWritten
	file.crc32 = hasher.Sum32()

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		cleanupTempFile(tmpFile)
		return nil, fmt.Errorf("seek temp file: %w", err)
	}
	return tmpFile, nil
}

// writeCompressedFile writes a compressed file to the ZIP archive and creates central directory entry
func (pzw *parallelZipWriter) writeCompressedFile(file *file, source io.Reader) error {
	meta := NewFileMetadata(file)
	meta.AddFilesystemExtraField()

	if err := pzw.zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if !file.isDir {
		pzw.zw.localHeaderOffset += file.compressedSize

		if err := pzw.zw.updateLocalHeader(file); err != nil {
			return fmt.Errorf("update header: %w", err)
		}

		if err := pzw.writeFileData(source); err != nil {
			return fmt.Errorf("write data: %w", err)
		}
	}

	if file.RequiresZip64() {
		meta.addZip64ExtraField()
	}

	return pzw.zw.addCentralDirEntry(file)
}

// writeFileData copies data from temporary location to final destination
func (pzw *parallelZipWriter) writeFileData(source io.Reader) error {
	if source == nil {
		return nil
	}
	if _, err := io.Copy(pzw.zw.dest, source); err != nil {
		return fmt.Errorf("copy temp file data: %w", err)
	}
	if mb, ok := source.(*MemoryBuffer); ok {
        mb.Reset()
        pzw.bufferPool.Put(mb)
    }
	return nil
}

// MemoryBuffer implements io.ReadWriteSeeker with in-memory storage
// It provides thread-safe operations suitable for parallel compression
type MemoryBuffer struct {
    data   []byte        // The underlying byte slice
    pos    int64         // Current read/write position
    mu     sync.RWMutex  // Protects concurrent access
    closed bool          // Whether the buffer is closed
}

// NewMemoryBuffer creates a new empty MemoryBuffer with optional initial capacity
func NewMemoryBuffer(capacity int) *MemoryBuffer {
    if capacity < 0 {
        capacity = 0
    }
    return &MemoryBuffer{
        data: make([]byte, 0, capacity),
        pos:  0,
    }
}

// Read reads up to len(p) bytes from the current position into p
// Implements io.Reader interface
func (mb *MemoryBuffer) Read(p []byte) (n int, err error) {
    mb.mu.RLock()
    defer mb.mu.RUnlock()

    if mb.closed {
        return 0, io.ErrClosedPipe
    }

    if mb.pos >= int64(len(mb.data)) {
        return 0, io.EOF
    }

    n = copy(p, mb.data[mb.pos:])
    mb.pos += int64(n)

    if n < len(p) {
        err = io.EOF
    }

    return n, err
}

// Write writes len(p) bytes from p to the buffer, expanding if necessary
// Implements io.Writer interface
func (mb *MemoryBuffer) Write(p []byte) (n int, err error) {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if mb.closed {
        return 0, io.ErrClosedPipe
    }

    // Calculate required capacity
    required := mb.pos + int64(len(p))
    if required > int64(cap(mb.data)) {
        // Grow buffer by at least doubling, but enough to fit required data
        newCap := max(int64(cap(mb.data))*2, required)
        if newCap < 64 {
            newCap = 64
        }
        newData := make([]byte, len(mb.data), newCap)
        copy(newData, mb.data)
        mb.data = newData
    }

    // Extend slice if writing beyond current length
    if required > int64(len(mb.data)) {
        mb.data = mb.data[:required]
    }

    // Copy data at current position
    n = copy(mb.data[mb.pos:], p)
    mb.pos += int64(n)

    return n, nil
}

// Seek sets the offset for the next Read or Write
// Implements io.Seeker interface
func (mb *MemoryBuffer) Seek(offset int64, whence int) (int64, error) {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if mb.closed {
        return 0, io.ErrClosedPipe
    }

    var newPos int64
    switch whence {
    case io.SeekStart:
        newPos = offset
    case io.SeekCurrent:
        newPos = mb.pos + offset
    case io.SeekEnd:
        newPos = int64(len(mb.data)) + offset
    default:
        return 0, errors.New("invalid whence")
    }

    if newPos < 0 {
        return 0, errors.New("negative position")
    }

    mb.pos = newPos
    return newPos, nil
}

// Close marks the buffer as closed and releases resources
// Subsequent operations will return io.ErrClosedPipe
func (mb *MemoryBuffer) Close() error {
    mb.mu.Lock()
    defer mb.mu.Unlock()
    mb.closed = true
    mb.data = nil // Allow GC to reclaim memory
    return nil
}

// Reset clears the buffer and resets position to 0
// Maintains existing capacity to avoid reallocations
func (mb *MemoryBuffer) Reset() {
    mb.mu.Lock()
    defer mb.mu.Unlock()
    mb.data = mb.data[:0]
    mb.pos = 0
}