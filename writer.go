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
)

// zipWriter handles the low-level writing of ZIP archive structure.
type zipWriter struct {
	mu               sync.RWMutex
	dest             io.Writer      // Target stream (usually a byteCountWriter)
	config           ZipConfig      // Archive-wide configuration settings
	factories        factoriesMap   // Registry of compressors factories
	compressors      compressorsMap // Registry of available compressors
	entriesNum       int            // Number of files written to the archive
	sizeOfCentralDir int64          // Cumulative size of central directory entries
	headerOffset     int64          // Current write position for local file headers
	centralDir       *spillBuffer   // Buffer for accumulating central directory before final write
}

// newZipWriter creates and initializes a new zipWriter instance.
func newZipWriter(config ZipConfig, factories factoriesMap, dest io.Writer) *zipWriter {
	return &zipWriter{
		dest:        dest,
		config:      config,
		factories:   factories,
		compressors: make(map[compressorKey]Compressor),
		centralDir:  newSpillBuffer(),
	}
}

// WriteFile processes and writes a single file to the archive.
// It automatically chooses between streaming and buffered write strategies.
func (zw *zipWriter) WriteFile(file *File) error {
	var err error
	_, isSeeker := zw.dest.(io.WriteSeeker)
	file.flags = 0

	// Optimization: If not seeking, use Data Descriptor (Stream Mode)
	// This avoids temp files for standard Deflate/Store operations.
	// Note: Encryption logic often requires known sizes or temp files for MAC calculation,
	// so we stick to buffering for encrypted files for now.
	canStream := !isSeeker &&
		file.config.EncryptionMethod == NotEncrypted &&
		file.config.CompressionMethod != Deflate64 // Deflate64 behaves oddly with DD sometimes

	shouldBuffer := !isSeeker && !canStream ||
		file.config.EncryptionMethod != NotEncrypted ||
		file.uncompressedSize == SizeUnknown && !canStream

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
func (zw *zipWriter) WriteCentralDirAndEndRecords() error {
	defer zw.centralDir.Close()

	if _, err := zw.centralDir.WriteTo(zw.dest); err != nil {
		return fmt.Errorf("write central directory: %w", err)
	}

	// Determine if ZIP64 end records are needed
	if zw.sizeOfCentralDir > math.MaxUint32 || zw.headerOffset > math.MaxUint32 || zw.entriesNum > math.MaxUint16 {
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

// writeStream writes file directly to destination.
// Efficient for small files on seekable storage.
func (zw *zipWriter) writeStream(file *File) error {
	_, isSeeker := zw.dest.(io.WriteSeeker)
	usesDataDescriptor := !isSeeker

	if usesDataDescriptor {
		file.flags |= 0x08 // Set Bit 3
	}

	if err := zw.writeFileHeader(file); err != nil {
		return err
	}

	if !file.isDir {
		if err := zw.encodeAndUpdateFile(file, zw.dest); err != nil {
			return err
		}
		zw.headerOffset += file.compressedSize
	}

	if isSeeker {
		if file.compressedSize > math.MaxUint32 || file.uncompressedSize > math.MaxUint32 {
			return errors.New("file too large for stream mode (zip64 required but data already written)")
		}
		return zw.updateLocalHeader(file)
	}

	return zw.writeDataDescriptor(file)
}

// writeWithTempFile compresses/encrypts to a temporary file, then copies to destination.
// This allows calculating exact CRC and sizes before writing the Local File Header.
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

func (zw *zipWriter) writeDataDescriptor(file *File) error {
	var buf []byte

	if file.RequiresZip64() {
		// ZIP64 Data Descriptor: Sig(4) + CRC(4) + Comp(8) + Uncomp(8)
		buf = make([]byte, 24)
		binary.LittleEndian.PutUint32(buf[0:4], 0x08074b50)
		binary.LittleEndian.PutUint32(buf[4:8], file.crc32)
		binary.LittleEndian.PutUint64(buf[8:16], uint64(file.compressedSize))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(file.uncompressedSize))
	} else {
		// Standard Data Descriptor: Sig(4) + CRC(4) + Comp(4) + Uncomp(4)
		buf = make([]byte, 16)
		binary.LittleEndian.PutUint32(buf[0:4], 0x08074b50)
		binary.LittleEndian.PutUint32(buf[4:8], file.crc32)
		binary.LittleEndian.PutUint32(buf[8:12], uint32(file.compressedSize))
		binary.LittleEndian.PutUint32(buf[12:16], uint32(file.uncompressedSize))
	}

	if _, err := zw.dest.Write(buf); err != nil {
		return fmt.Errorf("write data descriptor: %w", err)
	}
	
	// Track offset for Central Directory (though usually CD offset calculation handles this)
	zw.headerOffset += int64(len(buf))
	
	return nil
}

// encodeAndUpdateFile runs the compression/encryption pipeline.
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

// encodeToWriter selects the encoding strategy.
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

// encodeUnencrypted compresses data and calculates CRC on the fly.
func (zw *zipWriter) encodeUnencrypted(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	counter := &byteCountWriter{dest: dest}
	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	uncompressedSize, err := zw.compressTo(io.TeeReader(src, hasher), counter, comp)
	if err != nil {
		return encodingStats{}, err
	}

	return encodingStats{
		crc32:            hasher.Sum32(),
		uncompressedSize: uncompressedSize,
		compressedSize:   counter.bytesWritten,
	}, nil
}

// encodeWithSeeker pre-calculates CRC for encrypted files using a seeker.
func (zw *zipWriter) encodeWithSeeker(src io.ReadSeeker, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	var stats encodingStats

	hasher := crc32.NewIEEE()
	n, err := io.Copy(io.Discard, io.TeeReader(src, hasher))
	if err != nil {
		return stats, fmt.Errorf("calc crc: %w", err)
	}
	stats.uncompressedSize = n
	stats.crc32 = hasher.Sum32()

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return stats, fmt.Errorf("seek source: %w", err)
	}

	counter := &byteCountWriter{dest: dest}
	
	encryptor, err := zw.createEncryptor(counter, cfg, stats.crc32)
	if err != nil {
		return stats, err
	}

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		encryptor.Close()
		return stats, err
	}

	if _, err := zw.compressTo(src, encryptor, comp); err != nil {
		encryptor.Close()
		return stats, err
	}

	if err := encryptor.Close(); err != nil {
		return stats, fmt.Errorf("encrypt close: %w", err)
	}

	stats.compressedSize = counter.bytesWritten
	return zw.finalizeStats(stats, cfg), nil
}

// encodeWithTempFile pre-calculates CRC for encrypted files by compressing to temp first.
func (zw *zipWriter) encodeWithTempFile(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	var stats encodingStats
	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return stats, err
	}

	tmpFile, err := os.CreateTemp("", "gozip-*")
	if err != nil {
		return stats, err
	}
	defer cleanupTempFile(tmpFile)

	stats.uncompressedSize, err = zw.compressTo(io.TeeReader(src, hasher), tmpFile, comp)
	if err != nil {
		return stats, err
	}
	stats.crc32 = hasher.Sum32()

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return stats, fmt.Errorf("seek temp file: %w", err)
	}

	stats.compressedSize, err = zw.encryptAndCopy(tmpFile, dest, cfg, stats.crc32)
	if err != nil {
		return stats, err
	}

	return zw.finalizeStats(stats, cfg), nil
}

func (zw *zipWriter) compressTo(src io.Reader, dest io.Writer, comp Compressor) (int64, error) {
	n, err := comp.Compress(src, dest)
	if err != nil {
		return 0, fmt.Errorf("compress: %w", err)
	}
	return n, nil
}

// encryptAndCopy: Source -> Encryptor -> Dest. Returns bytes written to dest.
func (zw *zipWriter) encryptAndCopy(src io.Reader, dest io.Writer, cfg FileConfig, crc uint32) (int64, error) {
	counter := &byteCountWriter{dest: dest}

	encryptor, err := zw.createEncryptor(counter, cfg, crc)
	if err != nil {
		return 0, err
	}

	if _, err := io.Copy(encryptor, src); err != nil {
		encryptor.Close()
		return 0, fmt.Errorf("encrypt: %w", err)
	}

	if err := encryptor.Close(); err != nil {
		return 0, fmt.Errorf("encrypt close: %w", err)
	}

	return counter.bytesWritten, nil
}

func (zw *zipWriter) finalizeStats(s encodingStats, cfg FileConfig) encodingStats {
	if cfg.EncryptionMethod == AES256 {
		s.crc32 = 0
	}
	return s
}

// createEncryptor factory
func (zw *zipWriter) createEncryptor(dest io.Writer, cfg FileConfig, crc32Val uint32) (io.WriteCloser, error) {
	switch cfg.EncryptionMethod {
	case ZipCrypto:
		return newZipCryptoWriter(dest, cfg.Password, byte(crc32Val>>24))
	case AES256:
		return newAes256Writer(dest, cfg.Password)
	default:
		return nil, fmt.Errorf("%w: %d", ErrEncryption, cfg.EncryptionMethod)
	}
}

// writeFileHeader writes the Local File Header.
func (zw *zipWriter) writeFileHeader(file *File) error {
	file.localHeaderOffset = zw.headerOffset
	// Note: newZipHeaders should handle creating the header structure
	header := newZipHeaders(file).LocalHeader()

	if n, err := zw.dest.Write(header.Encode()); err != nil {
		return fmt.Errorf("write header: %w", err)
	} else {
		zw.headerOffset += int64(n)
	}

	return nil
}

// addCentralDirEntry adds a Central Directory record.
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

// updateLocalHeader seeks back to the Local Header to patch CRC and sizes.
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

// writeZip64EndHeaders writes the ZIP64 EOCD Record and Locator.
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

// resolveCompressor finds or instantiates a compressor.
func (zw *zipWriter) resolveCompressor(method CompressionMethod, level int) (Compressor, error) {
	key := compressorKey{method: method, level: level}

	zw.mu.RLock()
	cached, ok := zw.compressors[key]
	zw.mu.RUnlock()
	if ok {
		return cached, nil
	}

	zw.mu.Lock()
	defer zw.mu.Unlock()

	// Double-check: Another goroutine might have created it while we waited for the lock
	if cached, ok := zw.compressors[key]; ok {
		return cached, nil
	}

	var comp Compressor

	// Check for custom registered factories first
	if factory, ok := zw.factories[method]; ok {
		comp = factory(level)
	} else {
		// Fallback to built-in methods
		switch method {
		case Store:
			comp = new(StoredCompressor)
		case Deflate:
			comp = NewDeflateCompressor(level)
		default:
			return nil, fmt.Errorf("%w: %d", ErrAlgorithm, method)
		}
	}

	// Cache the result
	zw.compressors[key] = comp
	return comp, nil
}

// cleanupTempFile safely cleans up a temporary file
func cleanupTempFile(tmpFile *os.File) {
	if tmpFile != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}
}

// parallelZipWriter handles parallel compression and sequential writing.
type parallelZipWriter struct {
	zw              *zipWriter
	sem             chan struct{}
	memoryThreshold int64
	bufferPool      sync.Pool
	onFileProcessed func(*File, error)
}

func newParallelZipWriter(config ZipConfig, factories factoriesMap, dest io.Writer, workers int) *parallelZipWriter {
	return &parallelZipWriter{
		zw:              newZipWriter(config, factories, dest),
		sem:             make(chan struct{}, workers),
		memoryThreshold: 10 * 1024 * 1024, // 10MB
		bufferPool: sync.Pool{
			New: func() interface{} {
				return newMemoryBuffer(64 * 1024)
			},
		},
		onFileProcessed: config.OnFileProcessed,
	}
}

// zipResult holds the outcome of a compression job
type zipResult struct {
	file *File
	src  io.Reader // Compressed data stream
	err  error
}

// WriteFiles processes multiple files in parallel and writes them to the ZIP archive.
// It uses a back pressure mechanism to ensure memory usage remains bounded,
// even if files are processed out of order or vary significantly in size.
func (pzw *parallelZipWriter) WriteFiles(ctx context.Context, files []*File) []error {
	results := make([]chan zipResult, len(files))
	for i := range results {
		results[i] = make(chan zipResult, 1)
	}

	maxInFlight := cap(pzw.sem) * 2
	inflightSem := make(chan struct{}, maxInFlight)

	var wg sync.WaitGroup
	var errs []error

	go func() {
		// Feeds work into the system up to the inflight limit
		for i, f := range files {
			// Acquire In-Flight Slot ALWAYS.
			// This ensures the writer can always release it.
			select {
			case <-ctx.Done():
				// If cancelled, we still acquire to keep the accounting symmetrical
				// or we break the loop? If we break, the writer hangs waiting on results[i].
				// We MUST populate all results channels.
			case inflightSem <- struct{}{}:
			}
			
			// If context is dead, fast-path the result
			if ctx.Err() != nil {
				results[i] <- zipResult{file: f, err: ctx.Err()}
				close(results[i])
				continue
			}
			
			wg.Add(1)
			go func(idx int, f *File) {
				defer wg.Done()

				// Acquire CPU Semaphore (Blocks if all CPUs are busy)
				select {
				case <-ctx.Done():
					// Even if cancelled, we must send a result to unblock the reader
					results[idx] <- zipResult{file: f, err: ctx.Err()}
					close(results[idx])
					return
				case pzw.sem <- struct{}{}:
				}
				defer func() { <-pzw.sem }()

				// Compress
				src, err := pzw.compressFile(ctx, f)
				results[idx] <- zipResult{file: f, src: src, err: err}
				close(results[idx])
			}(i, f)
		}
	}()
	// We cheat slightly: we don't wg.Wait() the spawner because the writer loop 
	// knows exactly how many files to expect (len(files)).

	for _, resultChan := range results {
		// Wait for the specific file's result
		res, ok := <-resultChan
		if !ok {
			// Should not happen unless logic bug or extreme panic
			continue
		}

		if res.err != nil {
			if !errors.Is(res.err, context.Canceled) && !errors.Is(res.err, context.DeadlineExceeded) {
				errs = append(errs, fmt.Errorf("%s: %w", res.file.name, res.err))
			}
		} else {
			// Write to the actual ZIP stream
			// Check context again before expensive I/O
			if ctx.Err() != nil {
				pzw.cleanupBuf(res.src)
			} else {
				err := pzw.writeCompressedFile(res.file, res.src)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", res.file.name, err))
				} else if err = pzw.zw.addCentralDirEntry(res.file); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", res.file.name, err))
				}

				if pzw.onFileProcessed != nil {
					pzw.onFileProcessed(res.file, err)
				}
				pzw.cleanupBuf(res.src)
			}
		}
		<-inflightSem
	}

	wg.Wait()
	return errs
}

// compressFile compresses a single file to memory or temp file.
func (pzw *parallelZipWriter) compressFile(ctx context.Context, file *File) (io.Reader, error) {
	if file.isDir || file.uncompressedSize == 0 {
		return nil, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var fileBuffer io.ReadWriteSeeker

	// Use memory buffer for small files, temp file for large ones
	if file.uncompressedSize != SizeUnknown && file.uncompressedSize <= pzw.memoryThreshold {
		buffer := pzw.bufferPool.Get().(*memoryBuffer)
		if int(file.uncompressedSize) > cap(buffer.data) {
			pzw.bufferPool.Put(buffer)
			buffer = newMemoryBuffer(int(file.uncompressedSize))
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

	// Helper to cleanup on error
	cleanup := func() {
		pzw.cleanupBuf(fileBuffer)
	}

	if file.shouldCopyRaw() {
		src, err := file.sourceFunc()
		if err != nil {
			cleanup()
			return nil, err
		}

		if _, err := io.Copy(fileBuffer, src); err != nil {
			cleanup()
			return nil, fmt.Errorf("copy raw: %w", err)
		}
	} else {
		src, err := file.Open()
		if err != nil {
			cleanup()
			return nil, err
		}
		defer src.Close()

		// Encode to the buffer
		stats, err := pzw.zw.encodeToWriter(&contextReader{ctx: ctx, r: src}, fileBuffer, file.config)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("encode: %w", err)
		}

		file.uncompressedSize = stats.uncompressedSize
		file.compressedSize = stats.compressedSize
		file.crc32 = stats.crc32
	}

	if _, err := fileBuffer.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return fileBuffer, fmt.Errorf("seek buffer: %w", err)
	}

	return fileBuffer, nil
}

// writeCompressedFile copies the pre-compressed data to the main zip stream.
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
func (pzw *parallelZipWriter) cleanupBuf(buf interface{}) {
	if mb, ok := buf.(*memoryBuffer); ok {
		if int64(cap(mb.data)) > pzw.memoryThreshold {
			// Don't pool huge buffers
			mb.Close()
		} else {
			mb.Reset()
			pzw.bufferPool.Put(mb)
		}
	} else if tmpFile, ok := buf.(*os.File); ok {
		cleanupTempFile(tmpFile)
	}
}

// memoryBuffer implements an in-memory ReadWriteSeeker.
type memoryBuffer struct {
	data   []byte
	pos    int64
	closed bool
}

func newMemoryBuffer(capacity int) *memoryBuffer {
	return &memoryBuffer{data: make([]byte, 0, max(0, capacity))}
}

func (mb *memoryBuffer) Read(p []byte) (n int, err error) {
	if mb.closed {
		return 0, io.ErrClosedPipe
	}
	if mb.pos >= int64(len(mb.data)) {
		return 0, io.EOF
	}
	n = copy(p, mb.data[mb.pos:])
	mb.pos += int64(n)
	return n, nil
}

func (mb *memoryBuffer) Write(p []byte) (n int, err error) {
	if mb.closed {
		return 0, io.ErrClosedPipe
	}
	// Simple append optimization
	if mb.pos == int64(len(mb.data)) {
		mb.data = append(mb.data, p...)
		mb.pos += int64(len(p))
		return len(p), nil
	}

	// Grow/Overwrite logic
	required := mb.pos + int64(len(p))
	if required > int64(cap(mb.data)) {
		newCap := max(int64(cap(mb.data))*2, required)
		if newCap < 64 {
			newCap = 64
		}
		newData := make([]byte, len(mb.data), newCap)
		copy(newData, mb.data)
		mb.data = newData
	}
	if required > int64(len(mb.data)) {
		mb.data = mb.data[:required]
	}
	n = copy(mb.data[mb.pos:], p)
	mb.pos += int64(n)
	return n, nil
}

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

func (mb *memoryBuffer) Close() error {
	mb.closed = true
	mb.data = nil
	return nil
}

func (mb *memoryBuffer) Reset() {
	mb.data = mb.data[:0]
	mb.pos = 0
}

func addFilesystemExtraField(f *File) {
	if f.metadata == nil {
		return
	}
	if hasPreciseTimestamps(f.metadata) {
		f.SetExtraField(NTFSFieldTag, encodeNTFSExtraField(f.metadata))
	}
}

// defaultSpillThreshold defines the limit (10MB) before the Central Directory
// is moved from RAM to a temporary file.
const defaultSpillThreshold = 10 * 1024 * 1024

// spillBuffer is a write buffer that stores data in memory up to a threshold,
// then spills to a temporary file. This prevents OOM when creating archives
// with millions of files.
type spillBuffer struct {
	mem       *bytes.Buffer
	disk      *os.File
	threshold int64
	written   int64
}

func newSpillBuffer() *spillBuffer {
	return &spillBuffer{
		mem:       bytes.NewBuffer(make([]byte, 0, 64*1024)), // Start with 64KB capacity
		threshold: defaultSpillThreshold,
	}
}

// Write implements io.Writer.
func (sb *spillBuffer) Write(p []byte) (int, error) {
	n := len(p)
	sb.written += int64(n)

	// Case 1: Already on disk
	if sb.disk != nil {
		return sb.disk.Write(p)
	}

	// Case 2: Still in memory, fitting within threshold
	if int64(sb.mem.Len()+n) <= sb.threshold {
		return sb.mem.Write(p)
	}

	// Case 3: Overflow - spill to disk
	if err := sb.spillToDisk(); err != nil {
		return 0, err
	}

	// Write the new data to the file
	return sb.disk.Write(p)
}

// spillToDisk moves current memory content to a temp file
func (sb *spillBuffer) spillToDisk() error {
	f, err := os.CreateTemp("", "gozip-cd-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(f, sb.mem); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("spill to disk: %w", err)
	}

	sb.disk = f
	sb.mem = nil // Release memory to GC
	return nil
}

// WriteTo implements io.WriterTo. It copies the buffer content to the destination
// and cleans up any temporary resources.
func (sb *spillBuffer) WriteTo(w io.Writer) (int64, error) {
	// If in memory, just write out
	if sb.disk == nil {
		return sb.mem.WriteTo(w)
	}

	// If on disk, seek to start and copy
	if _, err := sb.disk.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek temp CD: %w", err)
	}

	n, err := io.Copy(w, sb.disk)

	// Close and remove the temp file immediately after flushing
	sb.Close()

	return n, err
}

// Close cleans up temporary files.
func (sb *spillBuffer) Close() error {
	if sb.disk != nil {
		sb.disk.Close()
		os.Remove(sb.disk.Name())
		sb.disk = nil
	}
	sb.mem = nil
	return nil
}

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
	binary.LittleEndian.PutUint16(data[2:4], 32)
	binary.LittleEndian.PutUint32(data[4:8], 0)
	binary.LittleEndian.PutUint16(data[8:10], 1)
	binary.LittleEndian.PutUint16(data[10:12], 24)
	binary.LittleEndian.PutUint64(data[12:20], mtime)
	binary.LittleEndian.PutUint64(data[20:28], atime)
	binary.LittleEndian.PutUint64(data[28:36], ctime)
	return data
}

func encodeAESExtraField(file *File) []byte {
	data := make([]byte, 11)
	binary.LittleEndian.PutUint16(data[0:2], AESEncryptionTag)
	binary.LittleEndian.PutUint16(data[2:4], 7)
	binary.LittleEndian.PutUint16(data[4:6], 0x0002) // Version 2
	data[6] = 'A'
	data[7] = 'E'
	data[8] = 0x03 // AES-256
	binary.LittleEndian.PutUint16(data[9:11], uint16(file.config.CompressionMethod))
	return data
}
