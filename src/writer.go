package gozip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sync"
)

// zipWriter handles the low-level writing of ZIP archive structure
type zipWriter struct {
	config            ZipConfig      // Archive config
	dest              io.WriteSeeker // Target stream for writing archive data
	compressors       *sync.Map      // Reference to reusable compressors map
	entriesNum        int            // Internal counter of written entries
	sizeOfCentralDir  int64          // Total size of central directory in bytes
	localHeaderOffset int64          // Current offset for local file headers
	centralDirBuf     *bytes.Buffer  // Buffer for accumulating central directory entries
}

// newZipWriter creates and initializes a new zipWriter instance
func newZipWriter(zip *Zip, dest io.WriteSeeker) *zipWriter {
	return &zipWriter{
		config:        zip.config,
		dest:          dest,
		compressors:   &zip.compressors,
		centralDirBuf: new(bytes.Buffer),
	}
}

// WriteFile processes and writes a single file to the archive
func (zw *zipWriter) WriteFile(file *file) error {
	var err error
	if file.uncompressedSize == 0 && !file.isDir || file.uncompressedSize > math.MaxUint32 {
		err = zw.writeBuffered(file)
	} else {
		err = zw.writeStream(file)
	}
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	addFilesystemExtraField(file)
	if file.RequiresZip64() {
		file.AddExtraField(zip64ExtraFieldTag, encodeZip64ExtraField(file))
	}

	return zw.addCentralDirEntry(file)
}

func (zw *zipWriter) writeStream(file *file) error {
	if err := zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if !file.isDir {
		src, err := file.openFunc()
		if err != nil {
			return fmt.Errorf("open source: %w", err)
		}

		stats, err := zw.encodeToWriter(src, zw.dest, file.config)
		if err != nil {
			return fmt.Errorf("encode to writer: %w", err)
		}

		file.uncompressedSize = stats.uncompressedSize
		file.compressedSize = stats.compressedSize
		file.crc32 = stats.crc32
		zw.localHeaderOffset += stats.compressedSize

		if err := zw.updateLocalHeader(file); err != nil {
			return fmt.Errorf("update header: %w", err)
		}
	}
	return nil
}

func (zw *zipWriter) writeBuffered(file *file) error {
	tmpFile, err := os.CreateTemp("", "zip-buffer-*")
	if err != nil {
		return err
	}
	defer cleanupTempFile(tmpFile)

	src, err := file.openFunc()
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}

	stats, err := zw.encodeToWriter(src, tmpFile, file.config)
	if err != nil {
		return err
	}

	file.uncompressedSize = stats.uncompressedSize
	file.compressedSize = stats.compressedSize
	file.crc32 = stats.crc32

	if err := zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(zw.dest, tmpFile); err != nil {
		return fmt.Errorf("copy buffer: %w", err)
	}
	zw.localHeaderOffset += stats.compressedSize
	return nil
}

type compressionStats struct {
	crc32            uint32
	compressedSize   int64
	uncompressedSize int64
}

func (zw *zipWriter) encodeToWriter(src io.Reader, dest io.Writer, cfg FileConfig) (compressionStats, error) {
	sizeCounter := &byteCountWriter{dest: dest}
	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return compressionStats{}, err
	}

	uncompressed, err := comp.Compress(io.TeeReader(src, hasher), sizeCounter)
	if err != nil {
		return compressionStats{}, fmt.Errorf("compress: %w", err)
	}

	return compressionStats{
		crc32:            hasher.Sum32(),
		uncompressedSize: uncompressed,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
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
		zw.entriesNum,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.localHeaderOffset),
		zw.config.Comment,
	)
	if _, err := zw.dest.Write(endOfCentralDir); err != nil {
		return fmt.Errorf("write end of central directory: %w", err)
	}
	return nil
}

// writeFileHeader writes the local file header
// for a file entry and update local header offset
func (zw *zipWriter) writeFileHeader(file *file) error {
	file.localHeaderOffset = zw.localHeaderOffset
	header := newZipHeaders(file).LocalHeader()
	n, err := zw.dest.Write(header.encode())
	if err != nil {
		return fmt.Errorf("write local header: %w", err)
	}
	zw.localHeaderOffset += int64(n)
	return nil
}

// addCentralDirEntry adds a central directory entry
// for a file and update size of central directory
func (zw *zipWriter) addCentralDirEntry(file *file) error {
	cdData := newZipHeaders(file).CentralDirEntry()
	n, err := zw.centralDirBuf.Write(cdData.encode())
	if err != nil {
		return fmt.Errorf("write central directory entry: %w", err)
	}
	zw.sizeOfCentralDir += int64(n)
	zw.entriesNum++
	return nil
}

// updateLocalHeader updates the local file header with actual compression results
func (zw *zipWriter) updateLocalHeader(file *file) error {
	// Seek to CRC position in local header
	if _, err := zw.dest.Seek(file.localHeaderOffset+14, io.SeekStart); err != nil {
		return fmt.Errorf("seek to CRC position: %w", err)
	}

	var buf [12]byte
	binary.LittleEndian.PutUint32(buf[0:4], file.crc32)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(min(math.MaxUint32, file.compressedSize)))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(min(math.MaxUint32, file.uncompressedSize)))

	if _, err := zw.dest.Write(buf[:]); err != nil {
		return fmt.Errorf("write CRC and sizes: %w", err)
	}

	if file.RequiresZip64() {
		return zw.writeZip64ExtraField(file)
	}

	if _, err := zw.dest.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek to end of the file: %w", err)
	}
	return nil
}

// writeZip64ExtraField writes ZIP64 extra field for large files
func (zw *zipWriter) writeZip64ExtraField(file *file) error {
	buf := make([]byte, 4, 28)
	binary.LittleEndian.PutUint16(buf[0:2], zip64ExtraFieldTag)

	var tmp [8]byte
	if file.uncompressedSize > math.MaxUint32 {
		binary.LittleEndian.PutUint64(tmp[:], uint64(file.uncompressedSize))
		buf = append(buf, tmp[:]...)
	}
	if file.compressedSize > math.MaxUint32 {
		binary.LittleEndian.PutUint64(tmp[:], uint64(file.compressedSize))
		buf = append(buf, tmp[:]...)
	}

	fieldSize := uint16(len(buf) - 4)
	binary.LittleEndian.PutUint16(buf[2:4], fieldSize)

	// Skip filename length
	if _, err := zw.dest.Seek(2, io.SeekCurrent); err != nil {
		return fmt.Errorf("seek to extra field length: %w", err)
	}

	var lenBuf [2]byte
	binary.LittleEndian.PutUint16(lenBuf[:], fieldSize+4)
	if _, err := zw.dest.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write extra field length: %w", err)
	}

	l := int64(len(file.getFilename()))
	if _, err := zw.dest.Seek(l, io.SeekCurrent); err != nil {
		return fmt.Errorf("seek past filename: %w", err)
	}

	if _, err := zw.dest.Write(buf); err != nil {
		return fmt.Errorf("write extra field: %w", err)
	}

	zw.localHeaderOffset += int64(len(buf))
	return nil
}

// writeZip64EndHeaders writes ZIP64 end of central directory record and locator
func (zw *zipWriter) writeZip64EndHeaders() error {
	zip64EndOfCentralDir := encodeZip64EndOfCentralDirectoryRecord(
		uint64(zw.entriesNum),
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

// resolveCompressor determines the correct compressor for a file.
func (zw *zipWriter) resolveCompressor(method CompressionMethod, lvl int) (Compressor, error) {
	if method == Stored {
		return &StoredCompressor{}, nil
	}

	key := fmt.Sprintf("%d::%d", method, lvl)
	if val, ok := zw.compressors.Load(key); ok {
		return val.(Compressor), nil
	}

	if method == Deflated {
		key := fmt.Sprintf("__deflate::%d", lvl)
		if val, ok := zw.compressors.Load(key); ok {
			return val.(Compressor), nil
		}
		actual, _ := zw.compressors.LoadOrStore(key, NewDeflateCompressor(lvl))
		return actual.(Compressor), nil
	}

	return nil, fmt.Errorf("unsupported compression method: %d", method)
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
	zw              *zipWriter
	sem             chan struct{}
	memoryThreshold int64
	bufferPool      sync.Pool
}

// newParallelZipWriter creates a new parallelZipWriter instance
func newParallelZipWriter(zip *Zip, dest io.WriteSeeker, workers int) *parallelZipWriter {
	return &parallelZipWriter{
		zw:              newZipWriter(zip, dest),
		sem:             make(chan struct{}, workers),
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
		file   *file
		source io.Reader
		err    error
	}, len(files))

	for _, f := range files {
		wg.Add(1)
		pzw.sem <- struct{}{}

		go func(f *file) {
			defer wg.Done()
			defer func() { <-pzw.sem }()

			source, err := pzw.compressFile(f)
			results <- struct {
				file   *file
				source io.Reader
				err    error
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

	var fileBuffer io.ReadWriteSeeker
	if file.uncompressedSize > 0 && file.uncompressedSize <= pzw.memoryThreshold {
		buffer := pzw.bufferPool.Get().(*memoryBuffer)

		if int(file.uncompressedSize) > cap(buffer.data) {
			pzw.bufferPool.Put(buffer)
			buffer = NewMemoryBuffer(int(file.uncompressedSize))
		} else {
			buffer.Reset()
		}
		fileBuffer = buffer
	} else {
		tmpFile, err := os.CreateTemp("", "zip-compress-*")
		if err != nil {
			return nil, err
		}
		fileBuffer = tmpFile
	}

	src, err := file.openFunc()
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}

	stats, err := pzw.zw.encodeToWriter(src, fileBuffer, file.config)
	if err != nil {
		return nil, err
	}

	if _, err := fileBuffer.Seek(0, io.SeekStart); err != nil {
		pzw.cleanupBuf(fileBuffer)
		return fileBuffer, fmt.Errorf("reset buffer position: %w", err)
	}

	file.uncompressedSize = stats.uncompressedSize
	file.compressedSize = stats.compressedSize
	file.crc32 = stats.crc32
	return fileBuffer, nil
}

// writeCompressedFile writes a compressed file to the ZIP archive and creates central directory entry
func (pzw *parallelZipWriter) writeCompressedFile(file *file, src io.Reader) error {
	if err := pzw.zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if !file.isDir {
		if err := pzw.zw.updateLocalHeader(file); err != nil {
			return fmt.Errorf("update header: %w", err)
		}

		if err := pzw.writeFileData(src); err != nil {
			return fmt.Errorf("write data: %w", err)
		}
	}

	addFilesystemExtraField(file)
	if file.RequiresZip64() {
		file.AddExtraField(zip64ExtraFieldTag, encodeZip64ExtraField(file))
	}

	return pzw.zw.addCentralDirEntry(file)
}

// writeFileData copies data from temporary location to final destination
func (pzw *parallelZipWriter) writeFileData(source io.Reader) error {
	if source == nil {
		return nil
	}
	if n, err := io.Copy(pzw.zw.dest, source); err != nil {
		return fmt.Errorf("copy temp file data: %w", err)
	} else {
		pzw.zw.localHeaderOffset += n
	}
	if mb, ok := source.(*memoryBuffer); ok {
		mb.Reset()
		pzw.bufferPool.Put(mb)
	}
	return nil
}

// cleanupBuf frees memoryBuffer or os.File from memory
func (pzw *parallelZipWriter) cleanupBuf(buf interface{}) {
	if mb, ok := buf.(*memoryBuffer); ok {
		if int64(cap(mb.data)) > pzw.memoryThreshold {
			mb.Close()
		} else {
			mb.Reset()
			pzw.bufferPool.Put(mb)
		}
	} else if tmpFile, ok := buf.(*os.File); ok {
		cleanupTempFile(tmpFile)
	}
}

// memoryBuffer implements io.ReadWriteSeeker with in-memory storage
// It provides thread-safe operations suitable for parallel compression
type memoryBuffer struct {
	data   []byte // The underlying byte slice
	pos    int64  // Current read/write position
	closed bool   // Whether the buffer is closed
}

// NewMemoryBuffer creates a new empty MemoryBuffer with optional initial capacity
func NewMemoryBuffer(capacity int) *memoryBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &memoryBuffer{
		data: make([]byte, 0, capacity),
		pos:  0,
	}
}

// Read reads up to len(p) bytes from the current position into p
// Implements io.Reader interface
func (mb *memoryBuffer) Read(p []byte) (n int, err error) {
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
func (mb *memoryBuffer) Write(p []byte) (n int, err error) {
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
func (mb *memoryBuffer) Seek(offset int64, whence int) (int64, error) {
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
func (mb *memoryBuffer) Close() error {
	mb.closed = true
	mb.data = nil // Allow GC to reclaim memory
	return nil
}

// Reset clears the buffer and resets position to 0
// Maintains existing capacity to avoid reallocations
func (mb *memoryBuffer) Reset() {
	mb.data = mb.data[:0]
	mb.pos = 0
}
