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
	localHeaderLen  = 30 // Size of local header without extensible fields
	directoryEndLen = 22 // Size of EOCD without comment
	zip64LocatorLen = 20 // Size of Zip64 Locator
)

// zipReader handles low-level reading of ZIP archive structure.
type zipReader struct {
	mu              sync.RWMutex
	src             io.ReaderAt         // Source stream for reading archive data
	fileSize        int64               // Total size of the archive
	decompressors   decompressorsMap    // Registry of available compressors
	textEncoder     func(string) string // Filename and comment encoder
	onFileProcessed func(*File, error)  // Callback after reading
}

// newZipReader creates and initializes a new zipReader instance.
// decompressors map can be nil - built-in Stored and Deflated decompressors are registered automatically.
func newZipReader(src io.ReaderAt, size int64, decompressors decompressorsMap, config ZipConfig) *zipReader {
	if decompressors == nil {
		decompressors = make(decompressorsMap)
	}
	decompressors[Stored] = new(StoredDecompressor)
	decompressors[Deflated] = new(DeflateDecompressor)

	return &zipReader{
		src:             src,
		fileSize:        size,
		decompressors:   decompressors,
		textEncoder:     config.TextEncoding,
		onFileProcessed: config.OnFileProcessed,
	}
}

// ReadFiles reads the ZIP archive and returns a list of files stored within it.
// It automatically handles both standard and ZIP64 format archives.
// Context is used to cancel the scanning process.
func (zr *zipReader) ReadFiles(ctx context.Context) ([]*File, error) {
	endDir, err := zr.findAndReadEndOfCentralDir(ctx)
	if err != nil {
		return nil, err
	}
	centralDirOffset, entriesNum := int64(endDir.CentralDirOffset), int64(endDir.TotalNumberOfEntries)

	if centralDirOffset == math.MaxUint32 || entriesNum == math.MaxUint16 {
		zip64EndDir, err := zr.findAndReadZip64EndOfCentralDir(ctx, endDir.CommentLength)
		if err != nil {
			return nil, err
		}
		centralDirOffset, entriesNum = int64(zip64EndDir.CentralDirOffset), int64(zip64EndDir.TotalNumberOfEntries)
	}

	return zr.readCentralDir(ctx, centralDirOffset, entriesNum)
}

// findAndReadEndOfCentralDir scans for the End of Central Directory record and reads it.
// Checks context cancellation during the scan loop.
func (zr *zipReader) findAndReadEndOfCentralDir(ctx context.Context) (internal.EndOfCentralDirectory, error) {
	var end internal.EndOfCentralDirectory

	if zr.fileSize < directoryEndLen {
		return end, fmt.Errorf("%w: file too small", ErrFormat)
	}

	const bufSize = 1024
	buf := make([]byte, bufSize)

	maxCommentLength := int64(math.MaxUint16)
	searchLimit := min(maxCommentLength+directoryEndLen, zr.fileSize)

	// Scan backwards
	for searchStart := int64(0); searchStart < searchLimit; {
		// Check context cancellation
		if err := ctx.Err(); err != nil {
			return end, err
		}

		readSize := min(bufSize, searchLimit-searchStart)
		readPos := zr.fileSize - searchLimit + searchStart

		if readPos < 0 {
			readPos = 0
			readSize = min(bufSize, zr.fileSize)
		}

		n, err := zr.src.ReadAt(buf[:readSize], readPos)
		if err != nil && err != io.EOF {
			return end, fmt.Errorf("read at %d: %w", readPos, err)
		}

		if n == 0 {
			break
		}

		// The active buffer is valid up to n bytes
		chunk := buf[:n]

		for p := n - 4; p >= 0; p-- {
			if binary.LittleEndian.Uint32(chunk[p:p+4]) == internal.EndOfCentralDirSignature {
				recordOffset := readPos + int64(p)

				// Ensure we can read the full 22-byte EOCD header
				if recordOffset+directoryEndLen > zr.fileSize {
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
		if int64(n) < 4 { // If we read less than signature size, we're done
			break
		}
	}

	return end, fmt.Errorf("%w: no end of central directory signature found", ErrFormat)
}

// findAndReadZip64EndOfCentralDir scans for the Zip64 End of Central Directory record.
func (zr *zipReader) findAndReadZip64EndOfCentralDir(ctx context.Context, commentLength uint16) (internal.Zip64EndOfCentralDirectory, error) {
	var zip64End internal.Zip64EndOfCentralDirectory

	if err := ctx.Err(); err != nil {
		return zip64End, err
	}

	zip64locatorOffset := zr.fileSize - int64(directoryEndLen+commentLength) - zip64LocatorLen
	if zip64locatorOffset < 0 {
		return zip64End, fmt.Errorf("%w: invalid zip64 locator offset", ErrFormat)
	}

	locReader := io.NewSectionReader(zr.src, zip64locatorOffset, zip64LocatorLen)
	if !zr.verifySignature(locReader, internal.Zip64EndOfCentralDirLocatorSignature) {
		return zip64End, fmt.Errorf("%w: expected zip64 end of central directory locator signature", ErrFormat)
	}

	zip64Locator, err := internal.ReadZip64EndOfCentralDirLocator(locReader)
	if err != nil {
		return zip64End, fmt.Errorf("read zip64 end of central dir locator: %w", err)
	}

	zip64EocdSize := zr.fileSize - int64(zip64Locator.Zip64EndOfCentralDirOffset)
	if zip64EocdSize < 0 {
		return zip64End, fmt.Errorf("%w: invalid zip64 end of central directory offset", ErrFormat)
	}

	zip64EocdReader := io.NewSectionReader(zr.src, int64(zip64Locator.Zip64EndOfCentralDirOffset), zip64EocdSize)
	if !zr.verifySignature(zip64EocdReader, internal.Zip64EndOfCentralDirSignature) {
		return zip64End, fmt.Errorf("%w: expected zip64 end of central directory signature", ErrFormat)
	}

	return internal.ReadZip64EndOfCentralDir(zip64EocdReader)
}

// readCentralDir reads the central directory entries starting at the specified offset.
// Checks context cancellation between entries.
func (zr *zipReader) readCentralDir(ctx context.Context, offset int64, entries int64) ([]*File, error) {
	safeCap := entries
	if safeCap > 1024*1024 {
		safeCap = 1024
	}
	files := make([]*File, 0, safeCap)

	cdReader := io.NewSectionReader(zr.src, offset, zr.fileSize-offset)

	for i := range entries {
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
	var isDir bool

	filename := decodeText(entry.Filename, entry.GeneralPurposeBitFlag, zr.textEncoder)
	comment := decodeText(entry.Comment, entry.GeneralPurposeBitFlag, zr.textEncoder)

	if strings.HasSuffix(filename, "/") {
		isDir = true
		filename = strings.TrimSuffix(filename, "/")
	}

	uncompressedSize := uint64(entry.UncompressedSize)
	compressedSize := uint64(entry.CompressedSize)
	localHeaderOffset := uint64(entry.LocalHeaderOffset)

	if zip64Data, ok := entry.ExtraField[Zip64ExtraFieldTag]; ok {
		pos := 0

		if uncompressedSize == math.MaxUint32 {
			if len(zip64Data) >= pos+8 {
				uncompressedSize = binary.LittleEndian.Uint64(zip64Data[pos : pos+8])
				pos += 8
			}
		}
		if compressedSize == math.MaxUint32 {
			if len(zip64Data) >= pos+8 {
				compressedSize = binary.LittleEndian.Uint64(zip64Data[pos : pos+8])
				pos += 8
			}
		}
		if localHeaderOffset == math.MaxUint32 {
			if len(zip64Data) >= pos+8 {
				localHeaderOffset = binary.LittleEndian.Uint64(zip64Data[pos : pos+8])
				pos += 8
			}
		}
	}

	var encryptionMethod EncryptionMethod
	compressionMethod := entry.CompressionMethod
	switch {
	case entry.CompressionMethod == winZipAESMarker:
		extraField := entry.ExtraField[AESEncryptionTag]
		compressionMethod = binary.LittleEndian.Uint16(extraField[9:11])
		encryptionMethod = AES256
	case (entry.GeneralPurposeBitFlag & 0x1) != 0:
		encryptionMethod = ZipCrypto
	}

	f := &File{
		name:              filename,
		isDir:             isDir,
		mode:              parseFileExternalAttributes(entry),
		uncompressedSize:  int64(uncompressedSize),
		compressedSize:    int64(compressedSize),
		crc32:             entry.CRC32,
		localHeaderOffset: int64(localHeaderOffset),
		hostSystem:        sys.HostSystem(entry.VersionMadeBy >> 8),
		modTime:           msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraField:        entry.ExtraField,
		config: FileConfig{
			CompressionMethod: CompressionMethod(compressionMethod),
			Name:              filename,
			Comment:           comment,
			EncryptionMethod:  encryptionMethod,
		},
	}
	f.sourceConfig = f.config
	f.openFunc = func() (io.ReadCloser, error) {
		return zr.openFile(f)
	}
	f.sourceFunc = func() (*io.SectionReader, error) {
		headerReader := io.NewSectionReader(zr.src, f.localHeaderOffset, localHeaderLen)

		buf := make([]byte, localHeaderLen)
		if _, err := io.ReadFull(headerReader, buf); err != nil {
			return nil, fmt.Errorf("read local header: %w", err)
		}

		if binary.LittleEndian.Uint32(buf[0:4]) != internal.LocalFileHeaderSignature {
			return nil, fmt.Errorf("%w: expected local file header signature", ErrFormat)
		}

		filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
		extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))
		dataOffset := f.localHeaderOffset + localHeaderLen + filenameLen + extraLen

		return io.NewSectionReader(zr.src, dataOffset, f.compressedSize), nil
	}

	return f
}

// openFile implements the openFunc for a File created from central directory.
// It reads the local file header, handles decryption if needed, and sets up
// decompression with CRC32 verification
func (zr *zipReader) openFile(f *File) (io.ReadCloser, error) {
	headerReader := io.NewSectionReader(zr.src, f.localHeaderOffset, localHeaderLen)

	buf := make([]byte, localHeaderLen)
	if _, err := io.ReadFull(headerReader, buf); err != nil {
		return nil, fmt.Errorf("read local header: %w", err)
	}

	if binary.LittleEndian.Uint32(buf[0:4]) != internal.LocalFileHeaderSignature {
		return nil, fmt.Errorf("%w: expected local file header signature", ErrFormat)
	}

	bitFlag := binary.LittleEndian.Uint16(buf[6:8])
	isEncrypted := bitFlag&0x1 != 0

	filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))
	dataOffset := f.localHeaderOffset + localHeaderLen + filenameLen + extraLen

	dataR := io.NewSectionReader(zr.src, dataOffset, f.compressedSize)

	var wrappedR io.Reader = dataR
	if isEncrypted {
		if f.config.Password == "" {
			return nil, fmt.Errorf("%w: file is encrypted but no password provided", ErrPasswordMismatch)
		}

		var err error
		switch f.config.EncryptionMethod {
		case ZipCrypto:
			_, dosTime := timeToMsDos(f.modTime)
			wrappedR, err = NewZipCryptoReader(dataR, f.config.Password, bitFlag, f.crc32, dosTime)
		case AES256:
			wrappedR, err = NewAes256Reader(dataR, f.config.Password, f.compressedSize)
		default:
			return nil, fmt.Errorf("%w: %d", ErrEncryption, f.config.EncryptionMethod)
		}
		if err != nil {
			return nil, err
		}
	}

	zr.mu.RLock()
	decompressor, ok := zr.decompressors[f.config.CompressionMethod]
	zr.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrAlgorithm, f.config.CompressionMethod)
	}

	rc, err := decompressor.Decompress(wrappedR)
	if err != nil {
		return nil, fmt.Errorf("decompress data: %w", err)
	}

	return &checksumReader{
		rc:   rc,
		hash: crc32.NewIEEE(),
		want: f.crc32,
		size: uint64(f.uncompressedSize),
	}, nil
}

// verifySignature checks whether the next 4 bytes match the given signature.
func (zr *zipReader) verifySignature(r io.Reader, s uint32) bool {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(buf) == s
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

// Read implements io.Reader interface while calculating CRC32 and tracking bytes read
func (cr *checksumReader) Read(p []byte) (int, error) {
	n, err := cr.rc.Read(p)
	if n > 0 {
		cr.read += uint64(n)
		if cr.read > cr.size {
			return n, ErrSizeMismatch
		}
		cr.hash.Write(p[:n])
	}
	return n, err
}

// Close implements io.Closer interface and verifies CRC32 and size after reading completes
func (cr *checksumReader) Close() error {
	defer cr.rc.Close()

	if cr.read != cr.size {
		return fmt.Errorf("%w: read %d, want %d", ErrSizeMismatch, cr.read, cr.size)
	}

	if got := cr.hash.Sum32(); got != cr.want {
		return fmt.Errorf("%w: got %x, want %x", ErrChecksum, got, cr.want)
	}
	return nil
}
