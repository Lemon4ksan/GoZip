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
	"os"
	"sync"

	"github.com/lemon4ksan/gozip/internal"
)

// zipWriter handles the low-level writing of ZIP archive structure.
type zipWriter struct {
	mu             sync.RWMutex
	dest           io.Writer      // Target stream (usually a byteCountWriter)
	config         ZipConfig      // Archive-wide configuration settings
	factories      factoriesMap   // Registry of compressors factories
	compressors    compressorsMap // Registry of available compressors
	entriesNum     int            // Number of files written to the archive
	centralDirSize int64          // Cumulative size of central directory entries
	headerOffset   int64          // Current write position for local file headers
	centralDir     *spillBuffer   // Buffer for accumulating central directory before final write
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
func (zw *zipWriter) WriteFile(f *File) error {
	f.flags = 0

	if f.isDir {
		if err := zw.writeFileHeader(f); err != nil {
			return err
		}
		return zw.addCentralDirEntry(f)
	}

	// Optimization: If not seeking, use Data Descriptor (Stream Mode)
	// This avoids temp files for standard Deflate/Store operations.
	// Note: Encryption logic often requires known sizes or temp files for MAC calculation,
	// so we stick to buffering for encrypted files for now.
	_, isSeeker := zw.dest.(io.WriteSeeker)
	canStreamDirectly := !isSeeker &&
		f.config.EncryptionMethod == NotEncrypted &&
		f.config.CompressionMethod != Deflate64 // Deflate64 behaves oddly with DD sometimes

	canSeekPatchHeader := isSeeker &&
		f.uncompressedSize != SizeUnknown &&
		f.config.EncryptionMethod == NotEncrypted

	var err error
	if canStreamDirectly || canSeekPatchHeader {
		err = zw.writeStream(f)
	} else {
		err = zw.writeTemp(f)
	}
	if err != nil {
		return err
	}

	return zw.addCentralDirEntry(f)
}

// WriteCentralDirAndEndRecords writes the central directory and end records.
func (zw *zipWriter) WriteCentralDirAndEndRecords() error {
	if err := zw.flushCentralDirectory(); err != nil {
		return err
	}

	if zw.requiresZip64() {
		if err := zw.writeZip64EndRecords(); err != nil {
			return err
		}
	}

	return zw.writeEOCD()
}

// flushCentralDirectory writes central directory contents to dest and closes the spillBuffer.
func (zw *zipWriter) flushCentralDirectory() error {
	defer zw.centralDir.Close()
	if _, err := zw.centralDir.WriteTo(zw.dest); err != nil {
		return fmt.Errorf("write central directory: %w", err)
	}
	return nil
}

// requiresZip64 returns true if variables exceed their standard limit.
func (zw *zipWriter) requiresZip64() bool {
	return zw.centralDirSize > StandardSizeLimit ||
		zw.headerOffset > StandardSizeLimit ||
		zw.entriesNum > StandardEntriesLimit
}

// writeZip64EndRecords writes the ZIP64 EOCD Record and Locator.
func (zw *zipWriter) writeZip64EndRecords() error {
	zip64EOCD := internal.EncodeZip64EOCDRecord(zw.entriesNum, zw.centralDirSize, zw.headerOffset)
	if _, err := zw.dest.Write(zip64EOCD); err != nil {
		return fmt.Errorf("write zip64 end of central directory: %w", err)
	}

	zip64EOCDLocator := internal.EncodeZip64EOCDLocator(zw.headerOffset + zw.centralDirSize)
	if _, err := zw.dest.Write(zip64EOCDLocator); err != nil {
		return fmt.Errorf("write zip64 end of central directory locator: %w", err)
	}

	return nil
}

// writeEOCD writes final zip records, finishing the archive creation.
func (zw *zipWriter) writeEOCD() error {
	eocd := internal.EncodeEOCD(
		zw.entriesNum,
		zw.centralDirSize,
		zw.headerOffset,
		zw.config.Comment,
	)
	if _, err := zw.dest.Write(eocd); err != nil {
		return fmt.Errorf("write end of central directory: %w", err)
	}
	return nil
}

// writeStream writes file directly to destination.
// Efficient for small files on seekable storage.
func (zw *zipWriter) writeStream(f *File) error {
	_, isSeeker := zw.dest.(io.WriteSeeker)

	usesDataDescriptor := !isSeeker
	if usesDataDescriptor {
		f.flags |= 0x08 // Set Bit 3
	}

	if f.config.CompressionMethod == Store {
		f.compressedSize = f.uncompressedSize
	}

	if err := zw.writeFileHeader(f); err != nil {
		return err
	}

	if err := zw.encodeToAndUpdate(f, zw.dest); err != nil {
		return err
	}
	zw.headerOffset += f.compressedSize

	if isSeeker {
		if f.compressedSize > StandardSizeLimit || f.uncompressedSize > StandardSizeLimit {
			// This can only happen if compressed size is greater than
			// uncompressed size, which is unexpected from compressor
			return errors.New("file too large for stream mode (zip64 field required but data already written)")
		}
		return zw.updateLocalHeader(f)
	}

	dd := internal.EncodeDataDescriptor(f.crc32, f.compressedSize, f.uncompressedSize)
	if n, err := zw.dest.Write(dd); err != nil {
		return fmt.Errorf("write data descriptor: %w", err)
	} else {
		zw.headerOffset += int64(n)
	}

	return nil
}

// writeTemp compresses/encrypts to a temporary file, then copies to destination.
// This allows calculating exact CRC and sizes before writing the Local File Header.
func (zw *zipWriter) writeTemp(f *File) error {
	temp, err := os.CreateTemp("", "gozip-*")
	if err != nil {
		return err
	}
	defer cleanupTmp(temp)

	if err := zw.encodeToAndUpdate(f, temp); err != nil {
		return err
	}

	if err := zw.writeFileHeader(f); err != nil {
		return err
	}

	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek buffer: %w", err)
	}
	if _, err := io.Copy(zw.dest, temp); err != nil {
		return fmt.Errorf("copy buffer: %w", err)
	}
	zw.headerOffset += f.compressedSize

	return nil
}

// encodeToAndUpdate coordinates the file processing pipeline and updates file with calculated sizes and crc.
func (zw *zipWriter) encodeToAndUpdate(f *File, dest io.Writer) error {
	if f.shouldCopyRaw() {
		src, err := f.srcFunc()
		if err != nil {
			return err
		}
		if _, err := io.Copy(dest, src); err != nil {
			return fmt.Errorf("copy raw: %w", err)
		}
		return nil
	}

	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	stats, err := zw.encodeTo(src, dest, f.config)
	if err != nil {
		return err
	}

	if f.uncompressedSize != SizeUnknown && f.uncompressedSize != stats.uncompressedSize {
		return ErrSizeMismatch
	}

	f.uncompressedSize = stats.uncompressedSize
	f.compressedSize = stats.compressedSize
	f.crc32 = stats.crc32

	return nil
}

type encodingStats struct {
	uncompressedSize int64
	compressedSize   int64
	crc32            uint32
}

// encodeTo routes processing to the appropriate strategy.
func (zw *zipWriter) encodeTo(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	// Strategy A: No Encryption (Fastest, Single Pass)
	if cfg.EncryptionMethod == NotEncrypted {
		return zw.encodeUnencrypted(src, dest, cfg)
	}

	// Strategy B: Encrypted + Seeker (Two Passes: CRC -> Compress+Encrypt)
	if seeker, ok := src.(io.ReadSeeker); ok {
		return zw.encodeEncryptedSeeker(seeker, dest, cfg)
	}

	// Strategy C: Encrypted + Stream (Compress to Temp -> Encrypt to Dest)
	return zw.encodeEncryptedStream(src, dest, cfg)
}

// Pipeline: Src -> Tee(Hasher) -> Compressor -> Counter -> Dest
func (zw *zipWriter) encodeUnencrypted(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	hasher := crc32.NewIEEE()
	counter := &byteCountWriter{dest: dest}

	input := io.TeeReader(src, hasher)

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	uncompressedSize, err := comp.Compress(input, counter)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress: %w", err)
	}

	return encodingStats{
		uncompressedSize: uncompressedSize,
		compressedSize:   counter.bytesWritten,
		crc32:            hasher.Sum32(),
	}, nil
}

// Pipeline: Src -> Tee(Hasher) -> Seek -> Encoding -> Dest
func (zw *zipWriter) encodeEncryptedSeeker(src io.ReadSeeker, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	size, crc, err := zw.calculateCRC(src)
	if err != nil {
		return encodingStats{}, err
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, fmt.Errorf("seek source: %w", err)
	}

	compressedSize, err := zw.writeEncrypted(src, dest, cfg, crc, true)
	if err != nil {
		return encodingStats{}, err
	}

	return zw.finalizeStats(encodingStats{
		uncompressedSize: size,
		compressedSize:   compressedSize,
		crc32:            crc,
	}, cfg), nil
}

// Pipeline: Src -> Tee(Hasher) -> Compressor -> TempFile -> Encoding -> Dest
func (zw *zipWriter) encodeEncryptedStream(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	tmpFile, err := os.CreateTemp("", "gozip-*")
	if err != nil {
		return encodingStats{}, err
	}
	defer cleanupTmp(tmpFile)

	hasher := crc32.NewIEEE()

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	uncompressedSize, err := comp.Compress(io.TeeReader(src, hasher), tmpFile)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress to temp: %w", err)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, fmt.Errorf("seek temp: %w", err)
	}

	// compress=false because data is already compressed in tmpFile
	compressedSize, err := zw.writeEncrypted(tmpFile, dest, cfg, hasher.Sum32(), false)
	if err != nil {
		return encodingStats{}, err
	}

	return zw.finalizeStats(encodingStats{
		uncompressedSize: uncompressedSize,
		compressedSize:   compressedSize,
		crc32:            hasher.Sum32(),
	}, cfg), nil
}

// writeEncrypted sets up the encryption pipeline and writes data through it.
// If compress is true, it wraps the encryptor with a compressor.
// If false, it just copies src to the encryptor (used when src is already compressed).
func (zw *zipWriter) writeEncrypted(src io.Reader, dest io.Writer, cfg FileConfig, crc uint32, compress bool) (int64, error) {
	counter := &byteCountWriter{dest: dest}

	encryptor, err := zw.createEncryptor(counter, cfg, crc)
	if err != nil {
		return 0, err
	}

	if compress {
		// Pipeline: Src -> Compressor -> Encryptor -> Counter -> Dest
		comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
		if err != nil {
			_ = encryptor.Close()
			return 0, err
		}
		if _, err := comp.Compress(src, encryptor); err != nil {
			_ = encryptor.Close()
			return 0, fmt.Errorf("compress/encrypt: %w", err)
		}
	} else {
		// Pipeline: Src -> Encryptor -> Counter -> Dest
		if _, err := io.Copy(encryptor, src); err != nil {
			_ = encryptor.Close()
			return 0, fmt.Errorf("encrypt copy: %w", err)
		}
	}

	if err := encryptor.Close(); err != nil {
		return 0, fmt.Errorf("encrypt close: %w", err)
	}

	return counter.bytesWritten, nil
}

// calculateCRC reads the entire stream to calculate size and CRC32.
func (zw *zipWriter) calculateCRC(r io.Reader) (int64, uint32, error) {
	hasher := crc32.NewIEEE()
	size, err := io.Copy(io.Discard, io.TeeReader(r, hasher))
	if err != nil {
		return 0, 0, fmt.Errorf("calc crc: %w", err)
	}
	return size, hasher.Sum32(), nil
}

func (zw *zipWriter) finalizeStats(s encodingStats, cfg FileConfig) encodingStats {
	// AES-256 does not store the CRC in the Local Header or Data Descriptor
	// (it uses the MAC for integrity), so we zero it out to match spec.
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
		return nil, fmt.Errorf("unknown encryption method: %d", cfg.EncryptionMethod)
	}
}

// writeFileHeader writes the Local File Header.
func (zw *zipWriter) writeFileHeader(f *File) error {
	if f.isDir && zw.config.UseImplicitDirs {
		return nil
	}

	f.localHeaderOffset = zw.headerOffset
	header := newZipHeaders(f).LocalHeader()

	if n, err := zw.dest.Write(header.Encode()); err != nil {
		return fmt.Errorf("write header: %w", err)
	} else {
		zw.headerOffset += int64(n)
	}

	return nil
}

// addCentralDirEntry adds a Central Directory record.
func (zw *zipWriter) addCentralDirEntry(f *File) error {
	if f.isDir && zw.config.UseImplicitDirs {
		return nil
	}

	if f.config.EncryptionMethod == AES256 {
		f.SetExtraField(AESEncryptionTag, internal.EncodeAESExtraField(uint16(f.config.CompressionMethod)))
	}
	if f.RequiresZip64() {
		f.SetExtraField(Zip64ExtraFieldTag, internal.EncodeZip64ExtraField(f.uncompressedSize, f.compressedSize, f.localHeaderOffset))
	}
	addFSExtraField(f)

	cdData := newZipHeaders(f).CentralDirEntry()

	if n, err := zw.centralDir.Write(cdData.Encode()); err != nil {
		return err
	} else {
		zw.centralDirSize += int64(n)
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

// cleanupTmp safely cleans up a temporary file
func cleanupTmp(f *os.File) {
	if f != nil {
		f.Close()
		os.Remove(f.Name())
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
	var threshold int64 = 10 * 1024 * 1024 // 10MB
	if config.MemoryThreshold > 0 {
		threshold = config.MemoryThreshold
	}

	return &parallelZipWriter{
		zw:              newZipWriter(config, factories, dest),
		sem:             make(chan struct{}, workers),
		memoryThreshold: threshold,
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
	file     *File
	src      io.Reader // Compressed data stream
	err      error
	acquired bool // Indicates if inflightSem slot was acquired
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
		for i, f := range files {
			pzw.spawnWorker(ctx, f, results[i], inflightSem, &wg)
		}
	}()

	var stopWriting bool
	for _, resultChan := range results {
		res, ok := <-resultChan
		if !ok {
			continue
		}

		if res.acquired {
			<-inflightSem
		}

		if res.err != nil {
			if ctx.Err() == nil {
				errs = append(errs, res.err)
			}
			stopWriting = true
			continue
		}

		if stopWriting {
			pzw.cleanupBuf(res.src)
			continue
		}

		if err := ctx.Err(); err != nil {
			stopWriting = true
			pzw.cleanupBuf(res.src)
			continue
		}

		err := pzw.writeCompressedFile(res.file, res.src)
		if err != nil {
			errs = append(errs, wrapErr("write", res.file, err))
			stopWriting = true
		} else {
			if err = pzw.zw.addCentralDirEntry(res.file); err != nil {
				errs = append(errs, wrapErr("write", res.file, err))
				stopWriting = true
			}
		}

		if pzw.onFileProcessed != nil {
			pzw.onFileProcessed(res.file, err)
		}

		pzw.cleanupBuf(res.src)
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errs
}

func (pzw *parallelZipWriter) spawnWorker(
	ctx context.Context,
	f *File,
	resChan chan<- zipResult,
	inflightSem chan struct{},
	wg *sync.WaitGroup,
) {
	acquired := false
	select {
	case <-ctx.Done():
	case inflightSem <- struct{}{}:
		acquired = true
	}

	if err := ctx.Err(); err != nil {
		resChan <- zipResult{file: f, err: err, acquired: acquired}
		close(resChan)
		return
	}

	wg.Go(func() {
		select {
		case <-ctx.Done():
			resChan <- zipResult{file: f, err: ctx.Err(), acquired: true}
			close(resChan)
			return
		case pzw.sem <- struct{}{}:
		}
		defer func() { <-pzw.sem }()

		src, err := pzw.compressFile(ctx, f)
		if err != nil {
			err = wrapErr("compress", f, err)
		}
		resChan <- zipResult{file: f, src: src, err: err, acquired: true}
		close(resChan)
	})
}

// compressFile compresses a single file to memory or temp file.
func (pzw *parallelZipWriter) compressFile(ctx context.Context, f *File) (io.Reader, error) {
	if f.isDir || f.uncompressedSize == 0 {
		return nil, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var fileBuffer io.ReadWriteSeeker

	// Use memory buffer for small files, temp file for large ones
	if f.uncompressedSize != SizeUnknown && f.uncompressedSize <= pzw.memoryThreshold {
		buffer := pzw.bufferPool.Get().(*memoryBuffer)
		if int(f.uncompressedSize) > cap(buffer.data) {
			pzw.bufferPool.Put(buffer)
			buffer = newMemoryBuffer(int(f.uncompressedSize))
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

	if f.shouldCopyRaw() {
		src, err := f.srcFunc()
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, err
		}

		if _, err := io.Copy(fileBuffer, src); err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, fmt.Errorf("copy raw: %w", err)
		}
	} else {
		src, err := f.Open()
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, err
		}
		defer src.Close()

		// Encode to the buffer
		stats, err := pzw.zw.encodeTo(&contextReader{ctx: ctx, r: src}, fileBuffer, f.config)
		if err != nil {
			pzw.cleanupBuf(fileBuffer)
			return nil, fmt.Errorf("encode: %w", err)
		}

		if f.uncompressedSize != SizeUnknown && stats.uncompressedSize != f.uncompressedSize {
			return nil, ErrSizeMismatch
		}

		f.uncompressedSize = stats.uncompressedSize
		f.compressedSize = stats.compressedSize
		f.crc32 = stats.crc32
	}

	if _, err := fileBuffer.Seek(0, io.SeekStart); err != nil {
		pzw.cleanupBuf(fileBuffer)
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

// cleanupBuf frees *memoryBuffer or os.File resources appropriately.
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
		cleanupTmp(tmpFile)
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

func addFSExtraField(f *File) {
	if f.metadata == nil {
		return
	}
	if hasPreciseTimestamps(f.metadata) {
		f.SetExtraField(NTFSFieldTag, internal.EncodeNTFSExtraField(f.metadata))
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
