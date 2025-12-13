// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sync"

	"github.com/lemon4ksan/gozip/internal"
	"github.com/lemon4ksan/gozip/internal/sys"
)

// zipWriter handles the low-level writing of ZIP archive structure.
type zipWriter struct {
	mu               sync.RWMutex
	dest             io.Writer      // Target stream for writing archive data
	config           ZipConfig      // Archive-wide configuration settings
	compressors      compressorsMap // Registry of available compressors
	entriesNum       int            // Number of files written to the archive
	sizeOfCentralDir int64          // Cumulative size of central directory entries
	headerOffset     int64          // Current write position for local file headers
	centralDir       *bytes.Buffer  // Buffer for accumulating central directory before final write
}

// newZipWriter creates and initializes a new zipWriter instance.
// compressors map can be nil and will be initialized automatically.
func newZipWriter(config ZipConfig, compressors compressorsMap, dest io.Writer) *zipWriter {
	if compressors == nil {
		compressors = make(compressorsMap)
	}

	return &zipWriter{
		dest:        dest,
		config:      config,
		compressors: compressors,
		centralDir:  new(bytes.Buffer),
	}
}

// WriteFile processes and writes a single file to the archive.
// It automatically chooses between streaming and buffered write strategies.
// based on file size, encryption requirements, and destination interface.
func (zw *zipWriter) WriteFile(file *File) error {
	var err error
	_, isSeeker := zw.dest.(io.WriteSeeker)
	shouldBuffer := !isSeeker || file.config.EncryptionMethod != NotEncrypted ||
		file.uncompressedSize > math.MaxUint32 || file.uncompressedSize == SizeUnknown

	if shouldBuffer {
		err = zw.writeWithTempFile(file)
	} else {
		err = zw.writeStream(file)
	}
	if err != nil {
		return err
	}

	return zw.addCentralDirEntry(file)
}

// WriteCentralDirAndEndRecords writes the central directory and end records.
// This must be called after all files have been written to complete the archive.
// It handles both standard and ZIP64 format based on archive size.
func (zw *zipWriter) WriteCentralDirAndEndRecords() error {
	if _, err := zw.dest.Write(zw.centralDir.Bytes()); err != nil {
		return fmt.Errorf("write central directory: %w", err)
	}

	if zw.sizeOfCentralDir > math.MaxUint32 || zw.headerOffset > math.MaxUint32 {
		if err := zw.writeZip64EndHeaders(); err != nil {
			return err
		}
	}

	endOfCentralDir := internal.EncodeEndOfCentralDirRecord(
		zw.entriesNum,
		uint64(zw.sizeOfCentralDir),
		uint64(zw.headerOffset),
		zw.config.Comment,
	)
	if _, err := zw.dest.Write(endOfCentralDir); err != nil {
		return fmt.Errorf("write end of central directory: %w", err)
	}

	return nil
}

// writeStream writes file directly to destination without intermediate storage.
// This is the most memory-efficient method but requires seekable output and small files.
// Not suitable for files requiring ZIP64 or encryption due to header size uncertainties.
func (zw *zipWriter) writeStream(file *File) error {
	if err := zw.writeFileHeader(file); err != nil {
		return err
	}

	if file.isDir {
		return nil
	}

	if err := zw.encodeAndUpdateFile(file, zw.dest); err != nil {
		return err
	}
	zw.headerOffset += file.compressedSize

	if file.compressedSize > math.MaxUint32 || file.uncompressedSize > math.MaxUint32 {
		return errors.New("file too large for stream mode (zip64 required but data already written)")
	}

	return zw.updateLocalHeader(file)
}

// writeWithTempFile uses temporary storage for files that can't be streamed directly.
// The file is compressed to a temp location first, then copied with proper headers.
func (zw *zipWriter) writeWithTempFile(file *File) error {
	var tmpFile *os.File
	var err error

	if !file.isDir {
		tmpFile, err = os.CreateTemp("", "gozip-*")
		if err != nil {
			return err
		}
		defer cleanupTempFile(tmpFile)

		if err := zw.encodeAndUpdateFile(file, tmpFile); err != nil {
			return err
		}
	}

	if err := zw.writeFileHeader(file); err != nil {
		return err
	}

	if tmpFile != nil {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek buffer: %w", err)
		}
		if _, err := io.Copy(zw.dest, tmpFile); err != nil {
			return fmt.Errorf("copy buffer: %w", err)
		}
		zw.headerOffset += file.compressedSize
	}

	return nil
}

// encodeAndUpdateFile compresses/encrypts file content and updates file metadata.
// It reads from the file source, processes through compression/encryption pipeline,
// and writes to the provided writer while collecting size and CRC information.
// It does not update local file header offset.
func (zw *zipWriter) encodeAndUpdateFile(file *File, writer io.Writer) error {
	if file.shouldCopyRaw() {
		src, err := file.sourceFunc()
		if err != nil {
			return err
		}

		if _, err := io.Copy(writer, src); err != nil {
			return fmt.Errorf("copy raw: %w", err)
		}
		return nil
	}

	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	stats, err := zw.encodeToWriter(src, writer, file.config)
	if err != nil {
		return err
	}

	file.uncompressedSize = stats.uncompressedSize
	file.compressedSize = stats.compressedSize
	file.crc32 = stats.crc32

	return nil
}

type encodingStats struct {
	uncompressedSize int64
	compressedSize   int64
	crc32            uint32
}

// encodeToWriter selects the optimal encoding strategy based on file configuration.
// It handles three main scenarios:
//   - Unencrypted files: direct compression with CRC calculation
//   - Encrypted seekable files: CRC pre-calculation followed by compression+encryption
//   - Encrypted non-seekable files: compression to temp file then encryption
func (zw *zipWriter) encodeToWriter(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	if cfg.EncryptionMethod == NotEncrypted {
		return zw.encodeUnencrypted(src, dest, cfg)
	}

	// If the source is a file, we read it twice: once for CRC, once for processing
	if seeker, ok := src.(io.ReadSeeker); ok {
		return zw.encodeWithSeeker(seeker, dest, cfg)
	}

	// We compress to a temp file first to calculate CRC, then encrypt the already compressed data
	return zw.encodeWithTempFile(src, dest, cfg)
}

// encodeUnencrypted handles compression without encryption.
// It calculates CRC32 during compression for integrity checking.
func (zw *zipWriter) encodeUnencrypted(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	sizeCounter := &byteCountWriter{dest: dest}
	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	uncompressed, err := comp.Compress(io.TeeReader(src, hasher), sizeCounter)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress: %w", err)
	}

	return encodingStats{
		crc32:            hasher.Sum32(),
		uncompressedSize: uncompressed,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
}

// encodeWithSeeker handles encrypted files with seekable source.
// Pre-calculates CRC by reading the entire file first, then processes normally.
func (zw *zipWriter) encodeWithSeeker(src io.ReadSeeker, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	hasher := crc32.NewIEEE()
	uncompressedSize, err := io.Copy(io.Discard, io.TeeReader(src, hasher))
	if err != nil {
		return encodingStats{}, fmt.Errorf("calc crc: %w", err)
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, fmt.Errorf("seek source: %w", err)
	}

	return zw.compressAndEncrypt(src, dest, cfg, hasher.Sum32(), uncompressedSize)
}

// encodeWithTempFile handles encrypted files with non-seekable source.
// Compresses to temporary storage first, then encrypts with calculated CRC.
func (zw *zipWriter) encodeWithTempFile(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	tmpFile, err := os.CreateTemp("", "gozip-*")
	if err != nil {
		return encodingStats{}, err
	}
	defer cleanupTempFile(tmpFile)

	uncompressedSize, err := comp.Compress(io.TeeReader(src, hasher), tmpFile)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress: %w", err)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, fmt.Errorf("seek temp file: %w", err)
	}

	return zw.encryptCompressed(tmpFile, dest, cfg, hasher.Sum32(), uncompressedSize)
}

// compressAndEncrypt processes raw data through compression then encryption pipeline.
// Used when we have pre-calculated CRC and know the uncompressed size.
func (zw *zipWriter) compressAndEncrypt(src io.Reader, dest io.Writer, cfg FileConfig,
	fileCRC uint32, uncompressedSize int64) (encodingStats, error) {

	sizeCounter := &byteCountWriter{dest: dest}

	encryptor, err := zw.createEncryptor(sizeCounter, cfg, fileCRC)
	if err != nil {
		return encodingStats{}, err
	}

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	_, err = comp.Compress(src, encryptor)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress: %w", err)
	}

	if c, ok := encryptor.(io.Closer); ok {
		c.Close()
	}

	if cfg.EncryptionMethod == AES256 {
		fileCRC = 0
	}

	return encodingStats{
		crc32:            fileCRC,
		uncompressedSize: uncompressedSize,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
}

// encryptCompressed applies encryption to already compressed data.
// Used when compression was done separately (e.g., to temp file).
func (zw *zipWriter) encryptCompressed(compressedSrc io.Reader, dest io.Writer, cfg FileConfig,
	fileCRC uint32, uncompressedSize int64) (encodingStats, error) {

	sizeCounter := &byteCountWriter{dest: dest}

	encryptor, err := zw.createEncryptor(sizeCounter, cfg, fileCRC)
	if err != nil {
		return encodingStats{}, err
	}
	defer func() {
		if c, ok := encryptor.(io.Closer); ok {
			c.Close()
		}
	}()

	if _, err := io.Copy(encryptor, compressedSrc); err != nil {
		return encodingStats{}, fmt.Errorf("encrypt: %w", err)
	}

	if cfg.EncryptionMethod == AES256 {
		fileCRC = 0
	}

	return encodingStats{
		crc32:            fileCRC,
		uncompressedSize: uncompressedSize,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
}

// createEncryptor instantiates the appropriate encryption writer based on configuration.
// ZipCrypto uses CRC MSB for password verification, AES uses salt-based verification.
func (zw *zipWriter) createEncryptor(dest io.Writer, cfg FileConfig, crc32Val uint32) (io.Writer, error) {
	switch cfg.EncryptionMethod {
	case ZipCrypto:
		return NewZipCryptoWriter(dest, cfg.Password, byte(crc32Val>>24))
	case AES256:
		return NewAes256Writer(dest, cfg.Password)
	default:
		return nil, fmt.Errorf("%w: %d", ErrEncryption, cfg.EncryptionMethod)
	}
}

// writeFileHeader writes the local file header for a file entry
// and updates the local header offset tracker.
func (zw *zipWriter) writeFileHeader(file *File) error {
	file.localHeaderOffset = zw.headerOffset
	header := newZipHeaders(file).LocalHeader()

	if n, err := zw.dest.Write(header.Encode()); err != nil {
		return fmt.Errorf("write header: %w", err)
	} else {
		zw.headerOffset += int64(n)
	}

	return nil
}

// addCentralDirEntry adds a central directory entry for a file
// and updates the central directory size tracker.
// Automatically adds ZIP64 extra fields if required by file sizes.
func (zw *zipWriter) addCentralDirEntry(file *File) error {
	if file.config.EncryptionMethod == AES256 {
		file.SetExtraField(AESEncryptionTag, encodeAESExtraField(file))
	}
	if file.RequiresZip64() {
		file.SetExtraField(Zip64ExtraFieldTag, encodeZip64ExtraField(file))
	}
	addFilesystemExtraField(file)

	cdData := newZipHeaders(file).CentralDirEntry()

	if n, err := zw.centralDir.Write(cdData.Encode()); err != nil {
		return err
	} else {
		zw.sizeOfCentralDir += int64(n)
		zw.entriesNum++
	}

	return nil
}

// updateLocalHeader updates the local file header with actual compression results.
// This is used in stream mode where sizes weren't known when header was written.
// Requires seekable destination to go back and update CRC/size fields.
func (zw *zipWriter) updateLocalHeader(file *File) error {
	ws, ok := zw.dest.(io.WriteSeeker)
	if !ok {
		return errors.New("dest must implement io.WriteSeeker interface")
	}

	// Seek to CRC position in local header
	if _, err := ws.Seek(file.localHeaderOffset+14, io.SeekStart); err != nil {
		return fmt.Errorf("seek to CRC position: %w", err)
	}

	var buf [12]byte
	binary.LittleEndian.PutUint32(buf[0:4], file.crc32)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(file.compressedSize))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(file.uncompressedSize))

	if _, err := ws.Write(buf[:]); err != nil {
		return fmt.Errorf("write CRC and sizes: %w", err)
	}

	if _, err := ws.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek to end of the file: %w", err)
	}
	return nil
}

// writeZip64EndHeaders writes ZIP64 end of central directory record and locator.
// Used when archive exceeds 4GB in total size or contains too many files.
func (zw *zipWriter) writeZip64EndHeaders() error {
	zip64EndOfCentralDir := internal.EncodeZip64EndOfCentralDirRecord(
		uint64(zw.entriesNum),
		uint64(zw.sizeOfCentralDir),
		uint64(zw.headerOffset),
	)
	if _, err := zw.dest.Write(zip64EndOfCentralDir); err != nil {
		return fmt.Errorf("write zip64 end of central directory: %w", err)
	}

	zip64EndOfCentralDirLocator := internal.EncodeZip64EndOfCentralDirLocator(
		uint64(zw.headerOffset + zw.sizeOfCentralDir),
	)
	if _, err := zw.dest.Write(zip64EndOfCentralDirLocator); err != nil {
		return fmt.Errorf("write zip64 end of central directory locator: %w", err)
	}

	return nil
}

// resolveCompressor determines the correct compressor for a file.
// Looks up custom compressors first, falls back to built-in Deflate/Store methods.
func (zw *zipWriter) resolveCompressor(method CompressionMethod, level int) (Compressor, error) {
	zw.mu.RLock()
	val, ok := zw.compressors[compressorKey{method: method, level: level}]
	zw.mu.RUnlock()
	if ok {
		return val, nil
	}

	switch method {
	case Stored:
		return new(StoredCompressor), nil
	case Deflated:
		zw.mu.Lock()
		defer zw.mu.Unlock()
		key := compressorKey{method: Deflated, level: level}

		// Double check if the key was just inserted
		if val, ok := zw.compressors[key]; ok {
			return val, nil
		}

		zw.compressors[key] = NewDeflateCompressor(level)
		return zw.compressors[key], nil
	default:
		return nil, fmt.Errorf("%w: %d", ErrAlgorithm, method)
	}
}

// cleanupTempFile safely cleans up a temporary file
func cleanupTempFile(tmpFile *os.File) {
	if tmpFile != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}
}

// parallelZipWriter handles parallel compression and writing of files to a ZIP archive.
type parallelZipWriter struct {
	zw              *zipWriter         // Underlying sequential writer
	sem             chan struct{}      // Semaphore for limiting concurrent workers
	memoryThreshold int64              // Size threshold for memory vs disk buffering
	bufferPool      sync.Pool          // Reusable memory buffers for small files
	onFileProcessed func(*File, error) // Callback after writing
}

// newParallelZipWriter creates a new parallelZipWriter instance.
// Workers parameter controls maximum concurrent compression operations.
func newParallelZipWriter(config ZipConfig, compressors compressorsMap, dest io.Writer, workers int) *parallelZipWriter {
	return &parallelZipWriter{
		zw:              newZipWriter(config, compressors, dest),
		sem:             make(chan struct{}, workers),
		memoryThreshold: 10 * 1024 * 1024, // 10MB default
		bufferPool: sync.Pool{
			New: func() interface{} {
				return NewMemoryBuffer(64 * 1024) // 64KB default
			},
		},
		onFileProcessed: config.OnFileProcessed,
	}
}

// WriteFiles processes multiple files in parallel and writes them to the ZIP archive.
// This method performs compression concurrently using a worker pool and writes
// files sequentially to maintain proper ZIP format structure.
// Returns a slice of errors encountered during processing.
func (pzw *parallelZipWriter) WriteFiles(ctx context.Context, files []*File) []error {
	var wg sync.WaitGroup
	results := make(chan struct {
		file *File
		src  io.Reader
		err  error
	}, len(files))

Loop:
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			break Loop
		}

		select {
		case <-ctx.Done():
			break Loop
		case pzw.sem <- struct{}{}:
		}

		wg.Add(1)
		go func(f *File) {
			defer wg.Done()
			defer func() { <-pzw.sem }()

			src, err := pzw.compressFile(ctx, f)
			results <- struct {
				file *File
				src  io.Reader
				err  error
			}{f, src, err}
		}(f)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []error
	for result := range results {
		if result.err != nil {
			if !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, context.DeadlineExceeded) {
				errs = append(errs, fmt.Errorf("%s: %w", result.file.name, result.err))
			}
			continue
		}

		if ctx.Err() != nil {
			pzw.cleanupBuf(result.src)
			continue
		}

		err := pzw.writeCompressedFile(result.file, result.src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, err))
		} else if err = pzw.zw.addCentralDirEntry(result.file); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, err))
		}

		if pzw.onFileProcessed != nil {
			pzw.onFileProcessed(result.file, err)
		}

		pzw.cleanupBuf(result.src)
	}
	return errs
}

// compressFile compresses a single file according to its configuration.
// Returns a reader containing the compressed data, using memory or temp file based on size.
func (pzw *parallelZipWriter) compressFile(ctx context.Context, file *File) (io.Reader, error) {
	if file.isDir || file.uncompressedSize == 0 {
		return nil, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var fileBuffer io.ReadWriteSeeker
	if file.uncompressedSize != SizeUnknown && file.uncompressedSize <= pzw.memoryThreshold {
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

	if file.shouldCopyRaw() {
		src, err := file.sourceFunc()
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, err
		}

		if _, err := io.Copy(fileBuffer, src); err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, fmt.Errorf("copy raw: %w", err)
		}
	} else {
		src, err := file.Open()
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, err
		}
		defer src.Close()

		stats, err := pzw.zw.encodeToWriter(&contextReader{ctx: ctx, r: src}, fileBuffer, file.config)
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, fmt.Errorf("encode: %w", err)
		}

		file.uncompressedSize = stats.uncompressedSize
		file.compressedSize = stats.compressedSize
		file.crc32 = stats.crc32
	}

	if _, err := fileBuffer.Seek(0, io.SeekStart); err != nil {
		pzw.cleanupBuf(fileBuffer)
		return fileBuffer, fmt.Errorf("seek buffer: %w", err)
	}

	return fileBuffer, nil
}

// writeCompressedFile writes a compressed file header and encoded data to the ZIP archive.
func (pzw *parallelZipWriter) writeCompressedFile(file *File, src io.Reader) error {
	if err := pzw.zw.writeFileHeader(file); err != nil {
		return err
	}

	if file.uncompressedSize == 0 {
		return nil
	}

	if n, err := io.Copy(pzw.zw.dest, src); err != nil {
		return fmt.Errorf("copy buffer data: %w", err)
	} else {
		pzw.zw.headerOffset += n
	}

	return nil
}

// cleanupBuf frees memoryBuffer or os.File resources appropriately.
// Memory buffers are returned to pool, temp files are deleted.
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
// and efficient memory management through pooling
type memoryBuffer struct {
	data   []byte // The underlying byte slice
	pos    int64  // Current read/write position
	closed bool   // Whether the buffer is closed
}

// NewMemoryBuffer creates a new empty MemoryBuffer with optional initial capacity
// If capacity is negative, it defaults to 0
func NewMemoryBuffer(capacity int) *memoryBuffer {
	return &memoryBuffer{data: make([]byte, 0, max(0, capacity))}
}

// Read reads up to len(p) bytes from the current position into p.
// Implements io.Reader interface.
// Returns io.EOF when buffer end is reached.
func (mb *memoryBuffer) Read(p []byte) (n int, err error) {
	if mb.closed {
		return 0, io.ErrClosedPipe
	}

	if mb.pos >= int64(len(mb.data)) {
		return 0, io.EOF
	}

	n = copy(p, mb.data[mb.pos:])
	mb.pos += int64(n)

	if mb.pos >= int64(len(mb.data)) && n > 0 {
		err = io.EOF
	}

	return n, err
}

// Write writes len(p) bytes from p to the buffer, expanding if necessary.
// Implements io.Writer interface.
// Grows buffer exponentially to amortize allocation costs.
func (mb *memoryBuffer) Write(p []byte) (n int, err error) {
	if mb.closed {
		return 0, io.ErrClosedPipe
	}

	// If at the end of the file
	if mb.pos == int64(len(mb.data)) {
		mb.data = append(mb.data, p...)
		mb.pos += int64(len(p))
		return len(p), nil
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

// Seek sets the offset for the next Read or Write.
// Implements io.Seeker interface.
// Supports all standard whence values: SeekStart, SeekCurrent, SeekEnd.
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

// Close marks the buffer as closed and releases resources.
// Subsequent operations will return io.ErrClosedPipe.
// The underlying buffer is set to nil to allow garbage collection.
func (mb *memoryBuffer) Close() error {
	mb.closed = true
	mb.data = nil // Allow GC to reclaim memory
	return nil
}

// Reset clears the buffer and resets position to 0.
// Maintains existing capacity to avoid reallocations.
// Useful for reusing buffers in pools.
func (mb *memoryBuffer) Reset() {
	mb.data = mb.data[:0]
	mb.pos = 0
}

// addFilesystemExtraField creates and sets extra fields with file metadata.
// Currently supports NTFS timestamps with nanosecond precision.
func addFilesystemExtraField(f *File) {
	if f.metadata == nil {
		return
	}

	if f.hostSystem == sys.HostSystemNTFS && hasPreciseTimestamps(f.metadata) {
		f.SetExtraField(NTFSFieldTag, encodeNTFSExtraField(f.metadata))
	}
}

// encodeZip64ExtraField generates ZIP64 extra field for files exceeding 4GB limits.
// Contains 64-bit versions of uncompressed size, compressed size, and local header offset.
// Only includes fields that actually exceed 32-bit limits to minimize overhead.
func encodeZip64ExtraField(f *File) []byte {
	data := make([]byte, 4, 28)
	binary.LittleEndian.PutUint16(data[0:2], Zip64ExtraFieldTag)

	if f.uncompressedSize > math.MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(f.uncompressedSize))
	}
	if f.compressedSize > math.MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(f.compressedSize))
	}
	if f.localHeaderOffset > math.MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(f.localHeaderOffset))
	}

	binary.LittleEndian.PutUint16(data[2:4], uint16(len(data)-4))
	return data
}

// encodeNTFSExtraField generates NTFS extra field with high-precision timestamps.
// Contains creation time, last access time, and last modification time in Windows FILETIME format.
func encodeNTFSExtraField(metadata map[string]interface{}) []byte {
	var mtime, atime, ctime uint64
	if val, ok := metadata["LastWriteTime"]; ok {
		if t, ok := val.(uint64); ok {
			mtime = t
		}
	}
	if val, ok := metadata["LastAccessTime"]; ok {
		if t, ok := val.(uint64); ok {
			atime = t
		}
	}
	if val, ok := metadata["CreationTime"]; ok {
		if t, ok := val.(uint64); ok {
			ctime = t
		}
	}

	// Tag(2) + Size(2) + Reserved(4) + Attr1(2) + Size1(2) + Mtime(8) + Atime(8) + Ctime(8)
	data := make([]byte, 36)

	binary.LittleEndian.PutUint16(data[0:2], NTFSFieldTag)
	binary.LittleEndian.PutUint16(data[2:4], 32)   // Block size
	binary.LittleEndian.PutUint32(data[4:8], 0)    // Reserved
	binary.LittleEndian.PutUint16(data[8:10], 1)   // Attribute1 (Tag 1)
	binary.LittleEndian.PutUint16(data[10:12], 24) // Size1 (Size of attributes)
	binary.LittleEndian.PutUint64(data[12:20], mtime)
	binary.LittleEndian.PutUint64(data[20:28], atime)
	binary.LittleEndian.PutUint64(data[28:36], ctime)

	return data
}

// encodeAESExtraField generates the WinZip AES Extra Field
func encodeAESExtraField(file *File) []byte {
	// Fixed size: 2+2+2+2+1+2 = 11 bytes header, 7 bytes data
	data := make([]byte, 11)

	// Header ID
	binary.LittleEndian.PutUint16(data[0:2], AESEncryptionTag)
	// Data Size (7 bytes)
	binary.LittleEndian.PutUint16(data[2:4], 7)
	// Version Number (0x0002 for AE-2)
	binary.LittleEndian.PutUint16(data[4:6], 0x0002)
	// Vendor ID ("AE")
	data[6] = 'A'
	data[7] = 'E'
	// AES Strength (0x03 for AES-256)
	data[8] = 0x03
	// Actual Compression Method
	binary.LittleEndian.PutUint16(data[9:11], uint16(file.config.CompressionMethod))

	return data
}

// encodeZip64LocalExtraField generates Zip64 field specifically for Local Header.
func encodeZip64LocalExtraField(f *File) []byte {
	// Fixed size: Tag(2) + Size(2) + Uncompressed(8) + Compressed(8) = 20 bytes
	data := make([]byte, 20)

	binary.LittleEndian.PutUint16(data[0:2], Zip64ExtraFieldTag)
	binary.LittleEndian.PutUint16(data[2:4], 16) // Size of payload
	binary.LittleEndian.PutUint64(data[4:12], uint64(f.uncompressedSize))
	binary.LittleEndian.PutUint64(data[12:20], uint64(f.compressedSize))

	return data
}
