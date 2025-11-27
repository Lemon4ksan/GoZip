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
	mu                sync.RWMutex
	dest              io.Writer             // Target stream for writing archive data
	config            ZipConfig             // Archive config
	compressors       map[string]Compressor // Reusable compressors map
	entriesNum        int                   // Internal counter of written entries
	sizeOfCentralDir  int64                 // Total size of central directory in bytes
	localHeaderOffset int64                 // Current offset for local file headers
	centralDirBuf     *bytes.Buffer         // Buffer for accumulating central directory entries
}

// newZipWriter creates and initializes a new zipWriter instance
func newZipWriter(config ZipConfig, compressors map[string]Compressor, dest io.Writer) *zipWriter {
	if compressors == nil {
		compressors = make(map[string]Compressor)
	}
	return &zipWriter{
		dest:          dest,
		config:        config,
		compressors:   compressors,
		centralDirBuf: new(bytes.Buffer),
	}
}

// WriteFile processes and writes a single file to the archive
func (zw *zipWriter) WriteFile(file *file) error {
	var err error
	_, isSeeker := zw.dest.(io.WriteSeeker)
	shouldBuffer := file.uncompressedSize == sizeUnknown ||
		file.uncompressedSize > math.MaxUint32 ||
		!isSeeker ||
		file.config.EncryptionMethod != NotEncrypted

	if file.config.EncryptionMethod == AES256 {
		file.AddExtraField(AESEncryptionTag, encodeAESExtraField(file))
	}
	if shouldBuffer {
		err = zw.writeBuffered(file)
	} else {
		err = zw.writeStream(file)
	}
	if err != nil {
		return err
	}

	return zw.addCentralDirEntry(file)
}

// WriteCentralDirAndEndRecords writes the central directory and end records
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

// WriteStream writes file directly into dest without
// creating temp files for storing compression result.
// It's not possible to stream files that require zip64 extra field
// since its exact size is unknown and it can override file data
func (zw *zipWriter) writeStream(file *file) error {
	if err := zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if !file.isDir {
		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("open source: %w", err)
		}
		defer src.Close()

		stats, err := zw.encodeToWriter(src, zw.dest, file.config)
		if err != nil {
			return fmt.Errorf("encode to writer: %w", err)
		}
		file.uncompressedSize = stats.uncompressedSize
		file.compressedSize = stats.compressedSize
		file.crc32 = stats.crc32
		zw.localHeaderOffset += stats.compressedSize

		if file.compressedSize > math.MaxUint32 || file.uncompressedSize > math.MaxUint32 {
			return errors.New("file too large for stream mode (zip64 required but header already written)")
		}

		if err := zw.updateLocalHeader(file); err != nil {
			return fmt.Errorf("update header: %w", err)
		}
	}
	return nil
}

func (zw *zipWriter) writeBuffered(file *file) error {
	var tmpFile *os.File
	var err error
	var compressedSize int64

	if !file.isDir {
		tmpFile, err = os.CreateTemp("", "zip-buffer-*")
		if err != nil {
			return err
		}
		defer cleanupTempFile(tmpFile)

		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("open source: %w", err)
		}
		defer src.Close()

		stats, err := zw.encodeToWriter(src, tmpFile, file.config)
		if err != nil {
			return fmt.Errorf("encode to writer: %w", err)
		}
		file.uncompressedSize = stats.uncompressedSize
		file.compressedSize = stats.compressedSize
		file.crc32 = stats.crc32
		compressedSize = stats.compressedSize
	}

	if err := zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	zw.localHeaderOffset += compressedSize

	if file.compressedSize > math.MaxUint32 || file.uncompressedSize > math.MaxUint32 {
		if err := zw.writeZip64ExtraField(file); err != nil {
			return fmt.Errorf("write zip64 extra field: %w", err)
		}
	}
	if file.HasExtraField(AESEncryptionTag) {
		n, err := zw.dest.Write(file.GetExtraField(AESEncryptionTag))
		if err != nil {
			return fmt.Errorf("write aes encryption extra field: %w", err)
		}
		zw.localHeaderOffset += int64(n)
	}

	if tmpFile != nil {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek buffer: %w", err)
		}
		if _, err := io.Copy(zw.dest, tmpFile); err != nil {
			return fmt.Errorf("copy buffer: %w", err)
		}
	}

	return nil
}

type encodingStats struct {
	crc32            uint32
	compressedSize   int64
	uncompressedSize int64
}

// encodeToWriter selects the best strategy for compression and encryption
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

func (zw *zipWriter) encodeWithSeeker(src io.ReadSeeker, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	hasher := crc32.NewIEEE()
	uncompressedSize, err := io.Copy(io.Discard, io.TeeReader(src, hasher))
	if err != nil {
		return encodingStats{}, fmt.Errorf("calc crc: %w", err)
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, fmt.Errorf("reset source: %w", err)
	}

	return zw.compressAndEncrypt(src, dest, cfg, hasher.Sum32(), uncompressedSize)
}

func (zw *zipWriter) encodeWithTempFile(src io.Reader, dest io.Writer, cfg FileConfig) (encodingStats, error) {
	hasher := crc32.NewIEEE()
	bufFile, err := os.CreateTemp("", "zip-cipher-*")
	if err != nil {
		return encodingStats{}, err
	}
	defer cleanupTempFile(bufFile)

	comp, err := zw.resolveCompressor(cfg.CompressionMethod, cfg.CompressionLevel)
	if err != nil {
		return encodingStats{}, err
	}

	uncompressedSize, err := comp.Compress(io.TeeReader(src, hasher), bufFile)
	if err != nil {
		return encodingStats{}, fmt.Errorf("compress to buffer: %w", err)
	}

	if _, err := bufFile.Seek(0, io.SeekStart); err != nil {
		return encodingStats{}, err
	}

	return zw.encryptCompressed(bufFile, dest, cfg, hasher.Sum32(), uncompressedSize)
}

// compressAndEncrypt takes RAW data, compresses it, and then encrypts
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
		return encodingStats{}, fmt.Errorf("compress & encrypt: %w", err)
	}

	if c, ok := encryptor.(io.Closer); ok {
		c.Close()
	}

	return encodingStats{
		crc32:            fileCRC,
		uncompressedSize: uncompressedSize,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
}

// encryptCompressed takes already compressed data and just encrypts it
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

	return encodingStats{
		crc32:            fileCRC,
		uncompressedSize: uncompressedSize,
		compressedSize:   sizeCounter.bytesWritten,
	}, nil
}

// createEncryptor factory
func (zw *zipWriter) createEncryptor(dest io.Writer, cfg FileConfig, crc32Val uint32) (io.Writer, error) {
	switch cfg.EncryptionMethod {
	case ZipCrypto:
		// ZipCrypto uses MSB of CRC32 for password verification
		return NewZipCryptoWriter(dest, cfg.Password, byte(crc32Val>>24))
	case AES256:
		// AES uses internal Salt for verification
		return NewAes256Writer(dest, cfg.Password)
	default:
		return nil, fmt.Errorf("unknown encryption method: %d", cfg.EncryptionMethod)
	}
}

// writeFileHeader writes the local file header
// for a file entry and update local header offset
func (zw *zipWriter) writeFileHeader(file *file) error {
	file.localHeaderOffset = zw.localHeaderOffset
	header := newZipHeaders(file).LocalHeader()
	n, err := zw.dest.Write(header.encode())
	if err != nil {
		return err
	}
	zw.localHeaderOffset += int64(n)
	return nil
}

// addCentralDirEntry adds a central directory entry
// for a file and updates size of central directory
func (zw *zipWriter) addCentralDirEntry(file *file) error {
	if file.RequiresZip64() {
		file.AddExtraField(Zip64ExtraFieldTag, encodeZip64ExtraField(file))
	}
	addFilesystemExtraField(file)

	cdData := newZipHeaders(file).CentralDirEntry()
	n, err := zw.centralDirBuf.Write(cdData.encode())
	if err != nil {
		return fmt.Errorf("write central directory entry: %w", err)
	}
	zw.sizeOfCentralDir += int64(n)
	zw.entriesNum++
	return nil
}

// updateLocalHeader updates the local file header
// with actual compression results for stream write
func (zw *zipWriter) updateLocalHeader(file *file) error {
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

// writeZip64ExtraField writes ZIP64 extra field
func (zw *zipWriter) writeZip64ExtraField(file *file) error {
	extraField := encodeZip64ExtraField(file)
	n, err := zw.dest.Write(extraField)
	if err != nil {
		return fmt.Errorf("write extra field: %w", err)
	}
	zw.localHeaderOffset += int64(n)
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
	zw.mu.RLock()
	key := fmt.Sprintf("%d::%d", method, lvl)
	val, ok := zw.compressors[key]
	zw.mu.RUnlock()
	if ok {
		return val, nil
	}

	if method == Deflated {
		zw.mu.Lock()
		defer zw.mu.Unlock()
		zw.compressors[key] = NewDeflateCompressor(lvl)
		return zw.compressors[key], nil
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
func newParallelZipWriter(config ZipConfig, compressors map[string]Compressor,
	 					  dest io.Writer, workers int) *parallelZipWriter {
	return &parallelZipWriter{
		zw:              newZipWriter(config, compressors, dest),
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
// This method performs compression concurrently using a worker pool and writes
// files sequentially to maintain proper ZIP format structure.
func (pzw *parallelZipWriter) WriteFiles(files []*file) []error {
	var wg sync.WaitGroup
	results := make(chan struct {
		file   *file
		src    io.Reader
		err    error
	}, len(files))

	for _, f := range files {
		wg.Add(1)
		pzw.sem <- struct{}{}

		go func(f *file) {
			defer wg.Done()
			defer func() { <-pzw.sem }()

			src, err := pzw.compressFile(f)
			results <- struct {
				file   *file
				src    io.Reader
				err    error
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
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, result.err))
			continue
		}
		if err := pzw.writeCompressedFile(result.file, result.src); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, err))
		} else if err := pzw.zw.addCentralDirEntry(result.file); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.file.name, err))
		}
		if tmpFile, ok := result.src.(*os.File); ok {
			cleanupTempFile(tmpFile)
		}
	}
	return errs
}

// compressFile compresses a single file according to its configuration
func (pzw *parallelZipWriter) compressFile(file *file) (io.Reader, error) {
	if file.isDir || file.uncompressedSize == 0 {
		return nil, nil
	}

	var fileBuffer io.ReadWriteSeeker
	if file.uncompressedSize != sizeUnknown && file.uncompressedSize <= pzw.memoryThreshold {
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

	src, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	stats, err := pzw.zw.encodeToWriter(src, fileBuffer, file.config)
	if err != nil {
		return nil, err
	}
	file.uncompressedSize = stats.uncompressedSize
	file.compressedSize = stats.compressedSize
	file.crc32 = stats.crc32

	if _, err := fileBuffer.Seek(0, io.SeekStart); err != nil {
		pzw.cleanupBuf(fileBuffer)
		return fileBuffer, fmt.Errorf("reset buffer position: %w", err)
	}
	return fileBuffer, nil
}

// writeCompressedFile writes a compressed file header and encoded data to the ZIP archive
func (pzw *parallelZipWriter) writeCompressedFile(file *file, src io.Reader) error {
	if file.config.EncryptionMethod == AES256 {
		file.AddExtraField(AESEncryptionTag, encodeAESExtraField(file))
	}

	if err := pzw.zw.writeFileHeader(file); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if file.uncompressedSize == 0 {
		return nil
	}
	if file.uncompressedSize > math.MaxUint32 || file.compressedSize > math.MaxUint32 {
		if err := pzw.zw.writeZip64ExtraField(file); err != nil {
			return fmt.Errorf("write zip64 extra field: %w", err)
		}
	}
	if file.HasExtraField(AESEncryptionTag) {
		n, err := pzw.zw.dest.Write(file.GetExtraField(AESEncryptionTag))
		if err != nil {
			return fmt.Errorf("write aes encryption extra field: %w", err)
		}
		pzw.zw.localHeaderOffset += int64(n)
	}
	if err := pzw.writeFileData(src); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// writeFileData copies data from temporary location to final destination
func (pzw *parallelZipWriter) writeFileData(src io.Reader) error {
	if src == nil {
		return nil
	}
	if n, err := io.Copy(pzw.zw.dest, src); err != nil {
		return fmt.Errorf("copy temp file data: %w", err)
	} else {
		pzw.zw.localHeaderOffset += n
	}
	if mb, ok := src.(*memoryBuffer); ok {
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

// addFilesystemExtraField creates and sets extra fields with file metadata
func addFilesystemExtraField(f *file) {
	if f.metadata == nil {
		return
	}

	if f.hostSystem == HostSystemNTFS && !f.HasExtraField(NTFSFieldTag) && hasPreciseTimestamps(f.metadata) {
		f.AddExtraField(NTFSFieldTag, encodeNTFSExtraField(f.metadata))
	}
}

func encodeZip64ExtraField(f *file) []byte {
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
func encodeAESExtraField(file *file) []byte {
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
