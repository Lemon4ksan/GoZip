// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
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

// zipReader handles low-level reading of ZIP archive structure.
type zipReader struct {
	mu              sync.RWMutex
	src             io.ReaderAt      // Source stream for reading archive data
	fileSize        int64            // Total size of the archive
	decompressors   decompressorsMap // Registry of available compressors
	password        string
	textEncoder     func(string) string // Filename and comment encoder
	onFileProcessed func(*File, error)  // Callback after reading
}

// newZipReader creates and initializes a new zipReader instance.
// decompressors map can be nil - built-in Stored and Deflated decompressors are registered automatically.
func newZipReader(src io.ReaderAt, size int64, dcm decompressorsMap, cfg ZipConfig) *zipReader {
	if dcm == nil {
		dcm = make(decompressorsMap)
	}
	if _, ok := dcm[Store]; !ok {
		dcm[Store] = new(StoredDecompressor)
	}
	if _, ok := dcm[Deflate]; !ok {
		dcm[Deflate] = new(DeflateDecompressor)
	}

	return &zipReader{
		src:             src,
		fileSize:        size,
		decompressors:   dcm,
		password:        cfg.Password,
		textEncoder:     cfg.TextEncoding,
		onFileProcessed: cfg.OnFileProcessed,
	}
}

// ReadFiles reads the ZIP archive and returns a list of files stored within it.
// It automatically handles both standard and ZIP64 format archives.
// Context is used to cancel the scanning process.
func (zr *zipReader) ReadFiles(ctx context.Context, eocd internal.EOCD) ([]*File, error) {
	offset, entriesNum := uint64(eocd.CentralDirOffset), uint64(eocd.EntriesNum)

	if eocd.CentralDirOffset == math.MaxUint32 || eocd.EntriesNum == math.MaxUint16 {
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

	const maxCommentLength = int64(math.MaxUint16)
	searchLimit := min(maxCommentLength+eocdSize, zr.fileSize)

	// Scan backwards from the end of the file
	for searchStart := int64(0); searchStart < searchLimit; {
		if err := ctx.Err(); err != nil {
			return internal.EOCD{}, err
		}

		readSize := min(bufSize, searchLimit-searchStart)
		readPos := zr.fileSize - searchLimit + searchStart

		if readPos < 0 {
			readPos = 0
			readSize = min(bufSize, zr.fileSize)
		}

		n, err := zr.src.ReadAt(buf[:readSize], readPos)
		if err != nil && err != io.EOF {
			return internal.EOCD{}, fmt.Errorf("read at %d: %w", readPos, err)
		}

		if n == 0 {
			break
		}

		// The active buffer is valid up to n bytes
		chunk := buf[:n]

		// Search for the signature in the chunk (backwards)
		for p := n - 4; p >= 0; p-- {
			if binary.LittleEndian.Uint32(chunk[p:p+4]) == internal.EOCDSignature {
				recordOffset := readPos + int64(p)

				// Ensure we can read the full 22-byte EOCD header
				if recordOffset+eocdSize > zr.fileSize {
					continue
				}

				// Calculate start of the record (skip signature 4 bytes)
				sr := io.NewSectionReader(zr.src, recordOffset+4, zr.fileSize-(recordOffset+4))
				return internal.ReadEndOfCentralDir(sr)
			}
		}

		// Move search window backwards
		// We subtract 3 to allow overlap for signatures that cross buffer boundaries
		searchStart += int64(n) - 3
		if int64(n) < 4 {
			break
		}
	}

	return internal.EOCD{}, fmt.Errorf("%w: no end of central directory signature found", ErrFormat)
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
	filename := decodeText(entry.Filename, entry.GeneralPurposeBitFlag, zr.textEncoder)
	comment := decodeText(entry.Comment, entry.GeneralPurposeBitFlag, zr.textEncoder)

	var isDir bool
	if strings.HasSuffix(filename, "/") {
		isDir = true
		filename = strings.TrimSuffix(filename, "/")
	}

	f := &File{
		name:              filename,
		isDir:             isDir,
		mode:              parseFileExternalAttributes(entry),
		uncompressedSize:  int64(entry.UncompressedSize),
		compressedSize:    int64(entry.CompressedSize),
		crc32:             entry.CRC32,
		localHeaderOffset: int64(entry.LocalHeaderOffset),
		hostSystem:        sys.HostSystem(entry.VersionMadeBy >> 8),
		modTime:           msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraFieldRaw:     entry.ExtraField,
	}

	compressionMethod, encryptionMethod := zr.parseExtraField(f, entry)
	f.config = FileConfig{
		CompressionMethod: compressionMethod,
		EncryptionMethod:  encryptionMethod,
		Password:          zr.password,
		Comment:           comment,
	}
	f.srcConfig = f.config

	f.openFunc = func() (io.ReadCloser, error) {
		return zr.openFile(f)
	}

	f.srcFunc = func() (*io.SectionReader, error) {
		return zr.openFileRaw(f)
	}

	return f
}

func (zr *zipReader) parseExtraField(f *File, entry internal.CentralDirectory) (CompressionMethod, EncryptionMethod) {
	var encryptionMethod EncryptionMethod
	compressionMethod := entry.CompressionMethod

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
			zr.parseZip64(f, data, entry)

		case AESEncryptionTag:
			if len(data) >= 7 {
				compressionMethod = binary.LittleEndian.Uint16(data[5:7])
				encryptionMethod = AES256
			}
		}

		offset += size
	}

	if (entry.GeneralPurposeBitFlag&0x1) != 0 && encryptionMethod == NotEncrypted {
		encryptionMethod = ZipCrypto
	}

	return CompressionMethod(compressionMethod), encryptionMethod
}

// openFile implements the logic to read a file from the archive.
// It locates the data, handles decryption, and initializes the decompressor.
func (zr *zipReader) openFile(f *File) (io.ReadCloser, error) {
	data, flags, err := zr.getRawDataStream(f)
	if err != nil {
		return nil, err
	}

	wrapped := data
	isEncrypted := flags&0x1 != 0

	if isEncrypted {
		wrapped, err = zr.wrapDecryption(data, f, flags)
		if err != nil {
			return nil, err
		}
	}

	rc, err := zr.wrapDecompression(wrapped, f.srcConfig.CompressionMethod)
	if err != nil {
		return nil, err
	}

	return newChecksumReader(rc, f), nil
}

func (zr *zipReader) getRawDataStream(f *File) (io.Reader, uint16, error) {
	headerReader := io.NewSectionReader(zr.src, f.localHeaderOffset, localHeaderSize)

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

	dataOffset := f.localHeaderOffset + localHeaderSize + filenameLen + extraLen

	return io.NewSectionReader(zr.src, dataOffset, f.compressedSize), flags, nil
}

// wrapDecryption wraps reader with decrypter based on method.
func (zr *zipReader) wrapDecryption(src io.Reader, f *File, flags uint16) (io.Reader, error) {
	if f.config.Password == "" {
		return nil, fmt.Errorf("%w: file is encrypted but no password provided", ErrPasswordMismatch)
	}

	switch f.srcConfig.EncryptionMethod {
	case ZipCrypto:
		_, dosTime := timeToMsDos(f.modTime)
		return newZipCryptoReader(src, f.config.Password, flags, f.crc32, dosTime)
	case AES256:
		return newAes256Reader(src, f.config.Password, f.compressedSize)
	default:
		return nil, fmt.Errorf("unknown encryption method: %d", f.srcConfig.EncryptionMethod)
	}
}

// wrapDecompression wraps reader with decompressor based on method.
func (zr *zipReader) wrapDecompression(src io.Reader, method CompressionMethod) (io.ReadCloser, error) {
	zr.mu.RLock()
	decompressor, ok := zr.decompressors[method]
	zr.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrAlgorithm, method)
	}

	return decompressor.Decompress(src)
}

// openFileRaw implements the logic to read raw file data from the archive without any decompression.
func (zr *zipReader) openFileRaw(f *File) (*io.SectionReader, error) {
	// We must read the Local Header to find the exact data offset, as
	// extra fields in Local Header may differ from Central Directory.
	headerReader := io.NewSectionReader(zr.src, f.localHeaderOffset, localHeaderSize)

	var buf [localHeaderSize]byte
	if _, err := io.ReadFull(headerReader, buf[:]); err != nil {
		return nil, fmt.Errorf("read local header: %w", err)
	}

	if binary.LittleEndian.Uint32(buf[0:4]) != internal.LocalFileHeaderSignature {
		return nil, fmt.Errorf("%w: expected local file header signature", ErrFormat)
	}

	filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))

	dataOffset := f.localHeaderOffset + localHeaderSize + filenameLen + extraLen

	return io.NewSectionReader(zr.src, dataOffset, f.compressedSize), nil
}

// verifySignature checks whether the next 4 bytes match the given signature.
func (zr *zipReader) verifySignature(r io.Reader, s uint32) bool {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(buf[:]) == s
}

// parseZip64 updates file with sizes from zip64 extra field.
func (zr *zipReader) parseZip64(f *File, data []byte, entry internal.CentralDirectory) {
	var pos int

	if entry.UncompressedSize == math.MaxUint32 {
		if len(data) >= pos+8 {
			f.uncompressedSize = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8
		}
	}
	if entry.CompressedSize == math.MaxUint32 {
		if len(data) >= pos+8 {
			f.compressedSize = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8
		}
	}
	if entry.LocalHeaderOffset == math.MaxUint32 {
		if len(data) >= pos+8 {
			f.localHeaderOffset = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8
		}
	}
}

func parseFileExternalAttributes(entry internal.CentralDirectory) fs.FileMode {
	var mode fs.FileMode
	hostSystem := sys.HostSystem(entry.VersionMadeBy >> 8)

	if hostSystem.IsUnix() {
		unixMode := uint32(entry.ExternalFileAttributes >> 16)
		mode = fs.FileMode(unixMode & 0777)

		switch unixMode & sys.S_IFMT {
		case sys.S_IFDIR:
			mode |= fs.ModeDir
		case sys.S_IFLNK:
			mode |= fs.ModeSymlink
		case sys.S_IFSOCK:
			mode |= fs.ModeSocket
		case sys.S_IFIFO:
			mode |= fs.ModeNamedPipe
		case sys.S_IFCHR:
			mode |= fs.ModeCharDevice
		case sys.S_IFBLK:
			mode |= fs.ModeDevice
		}
		return mode
	}

	if hostSystem.IsWindows() {
		isDir := strings.HasSuffix(entry.Filename, "/") || (entry.ExternalFileAttributes&0x10 != 0)

		if isDir {
			mode = 0755 | fs.ModeDir
		} else {
			mode = 0644
		}

		if entry.ExternalFileAttributes&0x01 != 0 {
			mode &^= 0222 // Remove write permission (a-w)
		}
		return mode
	}

	if strings.HasSuffix(entry.Filename, "/") {
		return 0755 | fs.ModeDir
	}
	return 0644
}

// checksumReader wraps an io.ReadCloser to verify CRC32 checksum and size during reading.
// It ensures data integrity by comparing computed hash with expected value upon closing.
type checksumReader struct {
	rc   io.ReadCloser
	hash hash.Hash32
	want uint32
	read uint64
	size uint64
}

func newChecksumReader(rc io.ReadCloser, f *File) *checksumReader {
	return &checksumReader{
		rc:   rc,
		hash: crc32.NewIEEE(),
		want: f.crc32,
		size: uint64(f.uncompressedSize),
	}
}

// Read implements io.Reader interface while calculating CRC32 and tracking bytes read.
func (cr *checksumReader) Read(p []byte) (int, error) {
	n, err := cr.rc.Read(p)
	if n > 0 {
		cr.read += uint64(n)
		// Fail fast if we read more than expected
		if cr.read > cr.size {
			return n, ErrSizeMismatch
		}
		cr.hash.Write(p[:n])
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
	if cr.read < cr.size {
		return nil
	}

	if cr.read > cr.size {
		return fmt.Errorf("%w: read %d, want %d", ErrSizeMismatch, cr.read, cr.size)
	}

	if got := cr.hash.Sum32(); got != cr.want {
		return fmt.Errorf("%w: got %x, want %x", ErrChecksum, got, cr.want)
	}
	return nil
}
