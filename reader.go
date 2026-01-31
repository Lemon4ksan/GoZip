// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/lemon4ksan/gozip/internal"
	"github.com/lemon4ksan/gozip/internal/sys"
)

const (
	localHeaderSize  = 30 // Size of local header without extensible fields
	eocdSize         = 22 // Size of EOCD without comment
	zip64LocatorSize = 20
)

// readerBase implements common reader operations
type readerBase struct {
	mu            sync.RWMutex
	password      string
	decompressors decompressorsMap    // Registry of available compressors
	textDecoder   func(string) string // Filename and comment decoder
}

func newReaderBase(dcm decompressorsMap, cfg ZipConfig) readerBase {
	if dcm == nil {
		dcm = make(decompressorsMap)
	}
	if _, ok := dcm[Store]; !ok {
		dcm[Store] = new(StoredDecompressor)
	}
	if _, ok := dcm[Deflate]; !ok {
		dcm[Deflate] = new(DeflateDecompressor)
	}

	return readerBase{
		decompressors: dcm,
		password:      cfg.Password,
		textDecoder:   cfg.TextEncoding,
	}
}

// verifySignature checks whether the next 4 bytes match the given signature.
func (rb *readerBase) verifySignature(r io.Reader, s uint32) bool {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(buf[:]) == s
}

// initPipeline wraps the raw data stream with decryption and decompression.
// raw: The source stream (either SectionReader for random access or LimitReader for streaming).
func (rb *readerBase) initPipeline(src io.Reader, f *File, flags uint16) (io.ReadCloser, error) {
	var wrapped io.Reader = src
	var err error

	if flags&0x1 != 0 {
		wrapped, err = rb.wrapDecryption(src, f, flags)
		if err != nil {
			return nil, err
		}
	}

	rc, err := rb.wrapDecompression(wrapped, f.srcConfig.CompressionMethod)
	if err != nil {
		return nil, err
	}

	return rc, nil
}

// wrapDecryption wraps reader with decrypter based on method.
func (rb *readerBase) wrapDecryption(src io.Reader, f *File, flags uint16) (io.Reader, error) {
	if f.srcConfig.Password == "" {
		return nil, fmt.Errorf("%w: file is encrypted but no password provided", ErrPasswordMismatch)
	}

	switch f.srcConfig.EncryptionMethod {
	case ZipCrypto:
		_, dosTime := timeToMsDos(f.modTime)
		return newZipCryptoReader(src, f.srcConfig.Password, flags, f.crc32, dosTime)
	case AES256:
		return newAes256Reader(src, f.srcConfig.Password, f.compressedSize)
	default:
		return nil, fmt.Errorf("unknown encryption method: %d", f.srcConfig.EncryptionMethod)
	}
}

// wrapDecompression wraps reader with decompressor based on method.
func (rb *readerBase) wrapDecompression(src io.Reader, method CompressionMethod) (io.ReadCloser, error) {
	rb.mu.RLock()
	decompressor, ok := rb.decompressors[method]
	rb.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrAlgorithm, method)
	}

	return decompressor.Decompress(src)
}

// parseZip64 updates file with sizes from central directory zip64 extra field.
func parseZip64(f *File, data []byte, entry internal.SharedEntry) {
	var pos int

	if entry.UncompressedSize == StandardSizeLimit {
		if len(data) >= pos+8 {
			f.uncompressedSize = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8
		}
	}
	if entry.CompressedSize == StandardSizeLimit {
		if len(data) >= pos+8 {
			f.compressedSize = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8
		}
	}
	if entry.LocalHeaderOffset == StandardSizeLimit {
		if len(data) >= pos+8 {
			f.localHeaderOffset = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		}
	}
}

// parseEntryConf sets the correct compression and encryption method based on entry fields and extra field data.
func parseEntryConf(f *File, entry internal.SharedEntry) {
	f.config.CompressionMethod = CompressionMethod(entry.CompressionMethod)

	if (entry.GeneralPurposeBitFlag & 0x1) != 0 {
		f.config.EncryptionMethod = ZipCrypto
	}

	for offset := 0; offset < len(entry.ExtraField); {
		if offset+4 > len(entry.ExtraField) {
			break
		}

		tag := binary.LittleEndian.Uint16(entry.ExtraField[offset : offset+2])
		size := int(binary.LittleEndian.Uint16(entry.ExtraField[offset+2 : offset+4]))

		offset += 4
		if offset+size > len(entry.ExtraField) {
			break
		}

		data := entry.ExtraField[offset : offset+size]

		switch tag {
		case Zip64ExtraFieldTag:
			f.hasZip64Extra = true
			parseZip64(f, data, entry)

		case NTFSFieldTag:
			if len(data) == 36 {
				f.metadata = internal.ParseNTFSExtraField(data)
			}
		case AESEncryptionTag:
			if len(data) >= 7 {
				meth := binary.LittleEndian.Uint16(data[5:7])
				f.config.CompressionMethod = CompressionMethod(meth)
				f.config.EncryptionMethod = AES256
			}
		}

		offset += size
	}

}

// zipReader handles low-level reading of ZIP archive structure.
type zipReader struct {
	readerBase
	src             io.ReaderAt        // Source stream for reading archive data
	fileSize        int64              // Total size of the archive
	onFileProcessed func(*File, error) // Callback after reading
}

// newZipReader creates and initializes a new zipReader instance.
// decompressors map can be nil - built-in Stored and Deflated decompressors are registered automatically.
func newZipReader(src io.ReaderAt, size int64, dcm decompressorsMap, cfg ZipConfig) *zipReader {
	return &zipReader{
		readerBase:      newReaderBase(dcm, cfg),
		src:             src,
		fileSize:        size,
		onFileProcessed: cfg.OnFileProcessed,
	}
}

// ReadFiles reads the ZIP archive and returns a list of files stored within it.
// It automatically handles both standard and ZIP64 format archives.
// Context is used to cancel the scanning process.
func (zr *zipReader) ReadFiles(ctx context.Context, eocd internal.EOCD) ([]*File, error) {
	offset, entriesNum := uint64(eocd.CentralDirOffset), uint64(eocd.EntriesNum)

	if eocd.CentralDirOffset == StandardSizeLimit || eocd.EntriesNum == StandardEntriesLimit {
		zip64EOCD, err := zr.findAndReadZip64EOCD(eocd.CommentLength)
		if err != nil {
			return nil, err
		}
		offset, entriesNum = zip64EOCD.CentralDirOffset, zip64EOCD.EntriesNum
	}

	return zr.readCentralDir(ctx, offset, entriesNum)
}

// FindAndReadEOCD scans for the End of Central Directory record and reads it.
// Checks context cancellation during the scan loop.
func (zr *zipReader) FindAndReadEOCD(ctx context.Context) (internal.EOCD, error) {
	if zr.fileSize < eocdSize {
		return internal.EOCD{}, fmt.Errorf("%w: file too small", ErrFormat)
	}

	const bufSize = 4096 // 4kb for modern SSDs
	var buf [bufSize]byte

	searchLen := min(int64(MaxStringLength+eocdSize), zr.fileSize)

	// Scan backwards from the end of the file
	for off := zr.fileSize; off > zr.fileSize-searchLen; {
		if err := ctx.Err(); err != nil {
			return internal.EOCD{}, err
		}

		readSize := int64(bufSize)
		readPos := off - readSize

		if readPos < zr.fileSize-searchLen {
			readPos = zr.fileSize - searchLen
			readSize = off - readPos
		}

		n, err := zr.src.ReadAt(buf[:readSize], readPos)
		if err != nil && err != io.EOF {
			return internal.EOCD{}, fmt.Errorf("read at %d: %w", readPos, err)
		}

		if eocd, found := zr.tryReadSignature(readSize, readPos, buf[:n]); found {
			return eocd, nil
		}

		// Move search window backwards
		// We subtract 3 to allow overlap for signatures that cross buffer boundaries
		off -= (readSize - 3)

		if readSize < 4 {
			break
		}
	}

	return internal.EOCD{}, fmt.Errorf("%w: no end of central directory signature found", ErrFormat)
}

func (zr *zipReader) tryReadSignature(readSize int64, readPos int64, buf []byte) (internal.EOCD, bool) {
	for p := readSize - 4; p >= 0; p-- {
		if binary.LittleEndian.Uint32(buf[p:p+4]) == internal.EOCDSignature {
			recordOffset := readPos + p

			// Ensure we can read the full 22-byte EOCD header
			if recordOffset+eocdSize > zr.fileSize {
				continue
			}

			expectedCommentLen := zr.fileSize - (recordOffset + eocdSize)
			if expectedCommentLen > MaxStringLength {
				continue
			}

			// Calculate start of the record (skip signature 4 bytes)
			sr := io.NewSectionReader(zr.src, recordOffset+4, zr.fileSize-(recordOffset+4))
			eocd, err := internal.ReadEOCD(sr)

			if err == nil && int64(eocd.CommentLength) == expectedCommentLen {
				return eocd, true
			}
		}
	}

	return internal.EOCD{}, false
}

// findAndReadZip64EOCD scans for the Zip64 End of Central Directory record.
func (zr *zipReader) findAndReadZip64EOCD(commentLength uint16) (internal.Zip64EOCD, error) {
	// Logic: EOCD (22+comment) -> Zip64 Locator (20) -> Zip64 EOCD
	zip64locatorOffset := zr.fileSize - int64(eocdSize+commentLength) - zip64LocatorSize
	if zip64locatorOffset < 0 {
		return internal.Zip64EOCD{}, fmt.Errorf("%w: invalid zip64 locator offset", ErrFormat)
	}

	locReader := io.NewSectionReader(zr.src, zip64locatorOffset, zip64LocatorSize)
	if !zr.verifySignature(locReader, internal.Zip64EOCDLocatorSignature) {
		return internal.Zip64EOCD{}, fmt.Errorf("%w: expected zip64 end of central directory locator signature", ErrFormat)
	}

	zip64Locator, err := internal.ReadZip64EOCDLocator(locReader)
	if err != nil {
		return internal.Zip64EOCD{}, fmt.Errorf("read zip64 end of central dir locator: %w", err)
	}

	zip64EOCDSize := zr.fileSize - int64(zip64Locator.Zip64EndOfCentralDirOffset)
	if zip64EOCDSize < 0 {
		return internal.Zip64EOCD{}, fmt.Errorf("%w: invalid zip64 end of central directory offset", ErrFormat)
	}

	zip64EOCDReader := io.NewSectionReader(zr.src, int64(zip64Locator.Zip64EndOfCentralDirOffset), zip64EOCDSize)
	if !zr.verifySignature(zip64EOCDReader, internal.Zip64EOCDSignature) {
		return internal.Zip64EOCD{}, fmt.Errorf("%w: expected zip64 end of central directory signature", ErrFormat)
	}

	return internal.ReadZip64EOCD(zip64EOCDReader)
}

// readCentralDir reads the central directory entries starting at the specified offset.
func (zr *zipReader) readCentralDir(ctx context.Context, offset uint64, entriesNum uint64) ([]*File, error) {
	// Cap initial allocation to avoid OOM on malformed files claiming huge entry counts
	safeCap := entriesNum
	if safeCap > 1024*1024 {
		safeCap = 1024
	}
	files := make([]*File, 0, safeCap)

	cdReader := io.NewSectionReader(zr.src, int64(offset), zr.fileSize-int64(offset))

	for i := range entriesNum {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !zr.verifySignature(cdReader, internal.CentralDirectorySignature) {
			return nil, fmt.Errorf("%w: expected central directory signature at entry %d", ErrFormat, i)
		}

		entry, err := internal.ReadCentralDirEntry(cdReader)
		if err != nil {
			return nil, fmt.Errorf("decode central dir entry: %w", err)
		}

		file := zr.newFileFromCentralDir(entry)
		files = append(files, file)

		if zr.onFileProcessed != nil {
			zr.onFileProcessed(file, nil)
		}
	}

	return files, nil
}

// newFileFromCentralDir creates a File struct from a central directory entry
func (zr *zipReader) newFileFromCentralDir(entry internal.CentralDirectory) *File {
	filename := decodeText(entry.Filename, entry.GeneralPurposeBitFlag, zr.textDecoder)
	comment := decodeText(entry.Comment, entry.GeneralPurposeBitFlag, zr.textDecoder)

	var isDir bool
	if strings.HasSuffix(filename, "/") {
		isDir = true
		filename = strings.TrimSuffix(filename, "/")
	}

	f := &File{
		name:              filename,
		isDir:             isDir,
		mode:              internal.ParseFileMode(entry),
		uncompressedSize:  int64(entry.UncompressedSize),
		compressedSize:    int64(entry.CompressedSize),
		crc32:             entry.CRC32,
		localHeaderOffset: int64(entry.LocalHeaderOffset),
		hostSystem:        sys.HostSystem(entry.VersionMadeBy >> 8),
		modTime:           msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraFieldRaw:     entry.ExtraField,
		config: FileConfig{
			Password: zr.password,
			Comment:  comment,
		},
	}

	shared := internal.SharedEntryFromCD(entry)
	parseEntryConf(f, shared)

	f.srcConfig = f.config

	f.openFunc = func() (io.ReadCloser, error) {
		return zr.openFile(f)
	}

	headerOffset, dataSize := f.localHeaderOffset, f.compressedSize
	f.srcFunc = func() (*io.SectionReader, error) {
		sr, _, err := zr.getRawDataStream(headerOffset, dataSize)
		return sr, err
	}

	return f
}

// openFile implements the logic to read a file from the archive.
// It locates the data, handles decryption, and initializes the decompressor.
func (zr *zipReader) openFile(f *File) (io.ReadCloser, error) {
	data, flags, err := zr.getRawDataStream(f.localHeaderOffset, f.compressedSize)
	if err != nil {
		return nil, err
	}

	rc, err := zr.initPipeline(data, f, flags)
	if err != nil {
		return nil, err
	}

	return newChecksumReader(rc, f), nil
}

// getRawDataStream wraps file data with io.SectionReader and returns file flags.
func (zr *zipReader) getRawDataStream(headerOffset, dataSize int64) (*io.SectionReader, uint16, error) {
	headerReader := io.NewSectionReader(zr.src, headerOffset, localHeaderSize)

	var buf [localHeaderSize]byte
	if _, err := io.ReadFull(headerReader, buf[:]); err != nil {
		return nil, 0, fmt.Errorf("read local header: %w", err)
	}

	if binary.LittleEndian.Uint32(buf[0:4]) != internal.LocalFileHeaderSignature {
		return nil, 0, fmt.Errorf("%w: expected local file header signature", ErrFormat)
	}

	flags := binary.LittleEndian.Uint16(buf[6:8])

	filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))

	dataOffset := headerOffset + localHeaderSize + filenameLen + extraLen

	return io.NewSectionReader(zr.src, dataOffset, dataSize), flags, nil
}

// checksumReader wraps an io.ReadCloser to verify CRC32 checksum and size during reading.
// It ensures data integrity by comparing computed hash with expected value upon closing.
type checksumReader struct {
	rc   io.ReadCloser
	hash hash.Hash32
	read uint64

	wantCRC  uint32
	wantSize uint64

	// Callback for streaming mode (Bit 3)
	// Called when the data in rc has run out (EOF).
	// It should read DD from the main stream and return an error if the CRC does not match.
	onEOF func(gotCRC uint32, gotSize uint64) error
}

func newChecksumReader(rc io.ReadCloser, f *File) *checksumReader {
	return &checksumReader{
		rc:       rc,
		hash:     crc32.NewIEEE(),
		wantCRC:  f.crc32,
		wantSize: uint64(f.uncompressedSize),
	}
}

// Read implements io.Reader interface while calculating CRC32 and tracking bytes read.
func (cr *checksumReader) Read(p []byte) (int, error) {
	n, err := cr.rc.Read(p)
	if n > 0 {
		cr.read += uint64(n)
		// Fail fast if we read more than expected
		if cr.wantSize > 0 && cr.read > cr.wantSize {
			return n, ErrSizeMismatch
		}
		cr.hash.Write(p[:n])
	}
	if err == io.EOF && cr.onEOF != nil {
		fn := cr.onEOF
		cr.onEOF = nil
		if cbErr := fn(cr.hash.Sum32(), cr.read); cbErr != nil {
			return n, cbErr
		}
	}
	return n, err
}

// Close implements io.Closer interface.
// It validates the checksum only if the entire file was read.
// Partial reads are allowed (e.g. for peeking) and do not trigger verification errors.
func (cr *checksumReader) Close() error {
	defer cr.rc.Close()

	// If we haven't read the whole file, we can't verify the checksum.
	// We do not return an error here to allow partial reading.
	if cr.read < cr.wantSize {
		return nil
	}

	if cr.read > cr.wantSize {
		return fmt.Errorf("%w: read %d, want %d", ErrSizeMismatch, cr.read, cr.wantSize)
	}

	if got := cr.hash.Sum32(); got != cr.wantCRC {
		return fmt.Errorf("%w: got %x, want %x", ErrChecksum, got, cr.wantCRC)
	}

	return nil
}

// StreamReader reads a ZIP archive sequentially from an [io.Reader].
//
// Unlike the standard [Zip] struct (which requires [io.ReaderAt]), StreamReader
// does not need random access to the data source. This makes it ideal for processing
// ZIP archives from network streams (e.g., HTTP response bodies) or pipes
// without buffering the entire file to disk or memory.
//
// Limitations:
//   - Since it reads Local File Headers instead of the Central Directory,
//     metadata usually stored at the end of the archive is incomplete.
//   - You can only read files in the order they appear in the stream.
//     You cannot go back to a previous file.
type StreamReader struct {
	readerBase
	src     io.Reader
	curFile *File
	br      *bufio.Reader
	opened  io.ReadCloser
	skipCRC bool
}

// NewStreamReader returns a new StreamReader reading from source.
func NewStreamReader(src io.Reader) *StreamReader {
	return &StreamReader{
		readerBase: newReaderBase(nil, ZipConfig{}),
		src:        src,
		br:         bufio.NewReaderSize(src, 32*1024),
	}
}

// NewStreamReader returns a new StreamReader reading from source with given password.
func NewStreamReaderWithPassword(src io.Reader, pwd string) *StreamReader {
	return &StreamReader{
		readerBase: newReaderBase(nil, ZipConfig{Password: pwd}),
		br:         bufio.NewReaderSize(src, 32*1024),
	}
}

// RegisterDecompressor adds support for reading a custom compression method.
// See [DeflateDecompressor] for implementation example.
func (sr *StreamReader) RegisterDecompressor(method CompressionMethod, d Decompressor) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.decompressors[method] = d
}

// SetPassword sets current password atomically.
func (sr *StreamReader) SetPassword(pwd string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.password = pwd
}

// SetTextDecoder sets current [TextDecoder] atomically.
func (sr *StreamReader) SetTextDecoder(td TextDecoder) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.textDecoder = td
}

// IsZipStream checks if the data stream starts with the ZIP local header signature.
// This is a necessary condition for [StreamReader] to work, as it cannot
// scan the file for the central directory.
func IsZipStream(r io.Reader) (bool, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	buf, err := br.Peek(4)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return binary.LittleEndian.Uint32(buf) == internal.LocalFileHeaderSignature, nil
}

// Next advances to the next entry in the ZIP archive.
//
// Usage:
//   - Returns the next [File] entry or [io.EOF] if the end of the archive is reached.
//   - If the previous file's data was not fully read, Next automatically discards
//     the remaining bytes to reach the next header.
//
// Behavior with Data Descriptors:
//   - If the previous file uses a Data Descriptor (bit 3 set) and the compression
//     method is Store, Next scans the stream for the signature (PK\07\08) if the exact size is unknown.
//   - For compressed data, it relies on the decompressor to find the end of the stream.
//   - The CRC32 checksum is verified only after the stream is fully consumed.
//
// Warning: The returned File object is populated from the Local File Header.
// Fields like Unix permissions, file comments, or precise NTFS timestamps are unavailable.
func (sr *StreamReader) Next() (*File, error) {
	if sr.curFile != nil {
		if err := sr.skipRemainingData(); err != nil {
			return nil, err
		}
	}

	var sig [4]byte
	if _, err := io.ReadFull(sr.br, sig[:]); err != nil {
		return nil, err
	}

	switch binary.LittleEndian.Uint32(sig[:]) {
	case internal.LocalFileHeaderSignature:
	case internal.CentralDirectorySignature:
		return nil, io.EOF
	default:
		return nil, ErrFormat
	}

	entry, err := internal.ReadLocalFileHeader(sr.br)
	if err != nil {
		return nil, err
	}

	file := sr.newFileFromLocalHeader(entry)
	sr.curFile = file
	return file, nil
}

// Open returns an [io.ReadCloser] that provides access to the decompressed content
// of the current file.
//
// Prerequisites:
//   - [StreamReader.Next] must be called successfully before calling Open.
//   - Open can be called only once per file.
//
// Behavior:
//   - Automatically handles decryption (if password is set in config) and decompression.
//   - If the file has a Data Descriptor (unknown size), the reader will read until
//     the decompression stream ends.
//   - Closing the returned ReadCloser is optional for the library's internal state
//     (Next will close it automatically), but recommended to free resources immediately.
//
// Returns error if no file is currently selected (Next wasn't called).
func (sr *StreamReader) Open() (io.ReadCloser, error) {
	raw, err := sr.OpenRaw()
	if err != nil {
		return nil, err
	}

	rc, err := sr.initPipeline(raw, sr.curFile, sr.curFile.flags)
	if err != nil {
		return nil, err
	}

	validator := newChecksumReader(rc, sr.curFile)

	if sr.curFile.flags&0x08 != 0 {
		validator.onEOF = func(gotCRC uint32, gotSize uint64) error {
			if err := sr.readDataDescriptor(); err != nil {
				return err
			}
			if gotCRC != sr.curFile.crc32 && !sr.skipCRC {
				return ErrChecksum
			}
			if gotSize != uint64(sr.curFile.uncompressedSize) && !sr.skipCRC {
				return ErrSizeMismatch
			}
			return nil
		}
	}

	sr.opened = validator
	return validator, nil
}

// OpenRaw returns an [io.Reader] for reading the raw content
// (compressed and/or encrypted) directly from the currently opened file.
// If the file is encrypted (AES), the reader includes Salt, PVV, and MAC bytes.
func (sr *StreamReader) OpenRaw() (io.Reader, error) {
	if sr.curFile == nil {
		return nil, errors.New("no current file")
	}

	if sr.curFile.config.CompressionMethod == Store {
		sr.curFile.compressedSize = sr.curFile.uncompressedSize
	}

	var raw io.Reader

	if sr.curFile.flags&0x8 != 0 && sr.curFile.compressedSize == 0 {
		if sr.curFile.config.CompressionMethod == Store {
			raw = newSignatureReader(sr.br, internal.DataDescriptorSignature) // Unsafe path
		} else {
			// The decompressor should automatically detect the end of the compressed block
			raw = sr.br
		}
	} else {
		raw = io.LimitReader(sr.br, sr.curFile.compressedSize)
	}

	return raw, nil
}

// Glob searches for the next file whose name matches the [path.Match] pattern.
// It automatically skips all intermediate files. Returns io.EOF if there are no more matches.
func (sr *StreamReader) Glob(pattern string) (*File, error) {
	pattern = strings.ReplaceAll(pattern, "\\", "/")

	if _, err := path.Match(pattern, ""); err != nil {
		return nil, err
	}

	for {
		f, err := sr.Next()
		if err != nil {
			return nil, err
		}
		matched, _ := path.Match(pattern, f.Name())
		if matched {
			return f, nil
		}
	}
}

// newFileFromCentralDir creates a File struct from a central directory entry.
func (sr *StreamReader) newFileFromLocalHeader(entry internal.LocalFileHeader) *File {
	filename := decodeText(entry.Filename, entry.GeneralPurposeBitFlag, sr.textDecoder)

	var isDir bool
	if strings.HasSuffix(filename, "/") {
		isDir = true
		filename = strings.TrimSuffix(filename, "/")
	}

	f := &File{
		name:             filename,
		isDir:            isDir,
		flags:            entry.GeneralPurposeBitFlag,
		uncompressedSize: int64(entry.UncompressedSize),
		compressedSize:   int64(entry.CompressedSize),
		crc32:            entry.CRC32,
		modTime:          msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraFieldRaw:    entry.ExtraField,
		config: FileConfig{
			Password: sr.password,
		},
	}

	shared := internal.SharedEntryFromLocal(entry)
	parseEntryConf(f, shared)
	f.srcConfig = f.config

	return f
}

// skipRemainingData discards any unread data of the current file and
// consumes the Data Descriptor if present.
func (sr *StreamReader) skipRemainingData() error {
	f := sr.curFile
	if f == nil {
		return nil
	}
	defer func() {
		sr.curFile = nil
		sr.opened = nil
	}()

	if sr.opened != nil {
		sr.skipCRC = true
		if _, err := io.Copy(io.Discard, sr.opened); err != nil {
			return fmt.Errorf("drain open stream: %w", err)
		}
		sr.opened.Close()
		sr.skipCRC = false
		return nil
	}

	if f.flags&0x8 != 0 {
		sigR := newSignatureReader(sr.br, internal.DataDescriptorSignature)
		if _, err := io.Copy(io.Discard, sigR); err != nil {
			return fmt.Errorf("scan for data descriptor: %w", err)
		}
		return sr.readDataDescriptor()
	}

	toSkip := f.compressedSize
	if _, err := sr.br.Discard(int(toSkip)); err != nil {
		if _, copyErr := io.CopyN(io.Discard, sr.br, toSkip); copyErr != nil {
			return fmt.Errorf("skip known size data: %w", copyErr)
		}
	}

	return nil
}

// readDataDescriptor parses the 16 or 24 byte record following the data.
func (sr *StreamReader) readDataDescriptor() error {
	var buf [24]byte

	if _, err := io.ReadFull(sr.br, buf[:4]); err != nil {
		return fmt.Errorf("read dd signature: %w", err)
	}

	off := 0
	var bytesToRead int
	if binary.LittleEndian.Uint32(buf[:4]) == internal.DataDescriptorSignature {
		off = 4
		bytesToRead = 12
		if sr.curFile.hasZip64Extra {
			bytesToRead = 20
		}
	} else {
		bytesToRead = 8
		if sr.curFile.hasZip64Extra {
			bytesToRead = 16
		}
	}

	if _, err := io.ReadFull(sr.br, buf[4:4+bytesToRead]); err != nil {
		return fmt.Errorf("read dd body: %w", err)
	}

	sr.curFile.crc32 = binary.LittleEndian.Uint32(buf[off : off+4])
	off += 4
	if sr.curFile.hasZip64Extra {
		sr.curFile.compressedSize = int64(binary.LittleEndian.Uint64(buf[off : off+8]))
		sr.curFile.uncompressedSize = int64(binary.LittleEndian.Uint64(buf[off+8 : off+16]))
	} else {
		sr.curFile.compressedSize = int64(binary.LittleEndian.Uint32(buf[off : off+4]))
		sr.curFile.uncompressedSize = int64(binary.LittleEndian.Uint32(buf[off+4 : off+8]))
	}

	return nil
}

// signatureReader reads from a buffered reader until a specific 4-byte signature is found.
// It stops before the signature, leaving it in the buffer for the next operation.
type signatureReader struct {
	r         *bufio.Reader
	signature [4]byte
	found     bool
	err       error
}

func newSignatureReader(r *bufio.Reader, sig uint32) *signatureReader {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], sig)
	return &signatureReader{
		r:         r,
		signature: b,
	}
}

func (sr *signatureReader) Read(p []byte) (n int, err error) {
	if sr.found {
		return 0, io.EOF
	}
	if sr.err != nil {
		return 0, sr.err
	}

	peekSize := len(p) + len(sr.signature)
	buf, peekErr := sr.r.Peek(peekSize)

	idx := bytes.Index(buf, sr.signature[:])
	if idx >= 0 {
		sr.found = true
		limit := idx

		if len(p) < limit {
			limit = len(p)
			sr.found = false
		}

		n, err = sr.r.Read(p[:limit])
		if sr.found {
			return n, io.EOF
		}
		return n, err
	}

	safeLen := len(buf) - len(sr.signature) + 1
	if safeLen <= 0 {
		if peekErr != nil {
			return 0, peekErr
		}
		return 0, nil
	}

	if len(p) < safeLen {
		safeLen = len(p)
	}

	return sr.r.Read(p[:safeLen])
}
