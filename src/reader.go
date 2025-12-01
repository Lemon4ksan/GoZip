package gozip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"strings"
	"sync"
)

const (
	localHeaderLen  = 30 // Size of local header without extensible fields
	directoryEndLen = 22 // Size of EOCD without comment
	zip64LocatorLen = 20 // Size of Zip64 Locator
)

// zipReader handles low-level reading of ZIP archive structure.
type zipReader struct {
	mu            sync.RWMutex
	src           io.ReadSeeker                      // Source stream for reading archive data
	decompressors map[CompressionMethod]Decompressor // Registry of available compressors
}

// newZipReader creates and initializes a new zipReader instance.
// decompressors map can be nil - built-in Stored and Deflated decompressors are registered automatically.
func newZipReader(src io.ReadSeeker, decompressors map[CompressionMethod]Decompressor) *zipReader {
	if decompressors == nil {
		decompressors = make(map[CompressionMethod]Decompressor)
	}
	decompressors[Stored] = new(StoredDecompressor)
	decompressors[Deflated] = new(DeflateDecompressor)

	return &zipReader{
		src:           src,
		decompressors: decompressors,
	}
}

// ReadFiles reads the ZIP archive and returns a list of files stored within it.
// It automatically handles both standard and ZIP64 format archives.
// The method reads the central directory and parses all file entries.
func (zr *zipReader) ReadFiles() ([]*File, error) {
	endDir, err := zr.findAndReadEndOfCentralDir()
	if err != nil {
		return nil, err
	}
	centralDirOffset, entriesNum := int64(endDir.CentralDirOffset), int64(endDir.TotalNumberOfEntries)

	if centralDirOffset == math.MaxUint32 || entriesNum == math.MaxUint16 {
		zip64EndDir, err := zr.findAndReadZip64EndOfCentralDir(endDir.CommentLength)
		if err != nil {
			return nil, err
		}
		centralDirOffset, entriesNum = int64(zip64EndDir.CentralDirOffset), int64(zip64EndDir.TotalNumberOfEntries)
	}

	if _, err := zr.src.Seek(centralDirOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to central dir offset: %w", err)
	}

	return zr.readCentralDir(entriesNum)
}

// findAndReadEndOfCentralDir scans for the End of Central Directory record and reads it.
// This record is located at the end of the ZIP file and contains metadata
// about the archive structure and central directory location.
func (zr *zipReader) findAndReadEndOfCentralDir() (endOfCentralDirectory, error) {
	fileSize, err := zr.src.Seek(0, io.SeekEnd)
	if err != nil {
		return endOfCentralDirectory{}, fmt.Errorf("get file size: %w", err)
	}

	if fileSize < directoryEndLen {
		return endOfCentralDirectory{}, errors.New("file too small to be a valid ZIP archive")
	}

	const bufSize = 1024
	buf := make([]byte, bufSize)

	maxCommentLength := int64(math.MaxUint16)
	searchLimit := min(maxCommentLength+directoryEndLen, fileSize)

	for searchStart := int64(0); searchStart < searchLimit; {
		// Calculate how much we need to read in this iteration
		readSize := min(bufSize, searchLimit-searchStart)
		
		// Calculate read position: start reading from (fileSize - searchLimit + searchStart)
		readPos := fileSize - searchLimit + searchStart
		
		// Ensure we don't read past file boundaries
		if readPos < 0 {
			readPos = 0
			readSize = min(bufSize, fileSize)
		}
		
		// Seek to read position
		if _, err := zr.src.Seek(readPos, io.SeekStart); err != nil {
			return endOfCentralDirectory{}, fmt.Errorf("seek to position %d: %w", readPos, err)
		}

		// Read data into buffer
		n, err := io.ReadFull(zr.src, buf[:readSize])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return endOfCentralDirectory{}, fmt.Errorf("read buffer: %w", err)
		}
		
		if n == 0 {
			break
		}

		// Scan buffer backwards for the signature
		// Start from the end of the buffer minus 3 (to check 4-byte signature)
		for p := n - 4; p >= 0; p-- {
			if binary.LittleEndian.Uint32(buf[p:p+4]) == __END_OF_CENTRAL_DIRECTORY_SIGNATURE {
				eocdStart := readPos + int64(p)

				// Ensure we can read the full 22-byte EOCD header
				if eocdStart+directoryEndLen > fileSize {
					continue
				}
				recordStart := readPos + int64(p) + 4
				if _, err := zr.src.Seek(recordStart, io.SeekStart); err != nil {
					return endOfCentralDirectory{}, fmt.Errorf("seek to record at %d: %w", recordStart, err)
				}
				return readEndOfCentralDir(zr.src)
			}
		}

		// Move search window backwards
		// We subtract 3 to allow overlap for signatures that cross buffer boundaries
		searchStart += readSize - 3
		if readSize < 4 { // If we read less than signature size, we're done
			break
		}
	}

	return endOfCentralDirectory{}, errors.New("couldn't locate end of central directory signature")
}

// findAndReadZip64EndOfCentralDir scans for the Zip64 End of Central Directory record.
// This record is located before End of Central Directory and its locator.
// It contains 64 bit values for Central Directory size and total number of entries.
func (zr *zipReader) findAndReadZip64EndOfCentralDir(commentLength uint16) (zip64EndOfCentralDirectory, error) {
	var zip64End zip64EndOfCentralDirectory
	fileSize, _ := zr.src.Seek(0, io.SeekEnd)

	zip64locatorOffset := fileSize - int64(directoryEndLen+commentLength) - zip64LocatorLen
	if _, err := zr.src.Seek(zip64locatorOffset, io.SeekStart); err != nil {
		return zip64End, fmt.Errorf("seek to zip64 locator: %w", err)
	}

	if !zr.verifySignature(__ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE) {
		return zip64End, errors.New("expected zip64 end of central directory locator signature")
	}

	zip64Locator, err := readZip64EndOfCentralDirLocator(zr.src)
	if err != nil {
		return zip64End, fmt.Errorf("read zip64 end of central dir locator: %w", err)
	}

	if _, err := zr.src.Seek(int64(zip64Locator.Zip64EndOfCentralDirOffset), io.SeekStart); err != nil {
		return zip64End, fmt.Errorf("seek to zip64 eocd: %w", err)
	}

	if !zr.verifySignature(__ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE) {
		return zip64End, errors.New("expected zip64 end of central directory signature")
	}

	return readZip64EndOfCentralDir(zr.src)
}

// readCentralDir reads the central directory entries starting at the specified offset.
// Each entry contains metadata about a file in the archive.
func (zr *zipReader) readCentralDir(entries int64) ([]*File, error) {
	safeCap := entries
	if safeCap > 1024*1024 {
		safeCap = 1024 
	}
	files := make([]*File, 0, safeCap)

	for i := range entries {
		if !zr.verifySignature(__CENTRAL_DIRECTORY_SIGNATURE) {
			return nil, fmt.Errorf("expected central directory signature at entry %d", i)
		}

		entry, err := readCentralDirEntry(zr.src)
		if err != nil {
			return nil, fmt.Errorf("decode central dir entry: %w", err)
		}

		files = append(files, zr.newFileFromCentralDir(entry))
	}

	return files, nil
}

// newFileFromCentralDir creates a File struct from a central directory entry
func (zr *zipReader) newFileFromCentralDir(entry centralDirectory) *File {
	var isDir bool

	filename := decodeText(entry.Filename, entry.GeneralPurposeBitFlag)
	comment := decodeText(entry.Comment, entry.GeneralPurposeBitFlag)

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
		hostSystem:        HostSystem(entry.VersionMadeBy >> 8),
		modTime:           msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraField:        entry.ExtraField,
		config: FileConfig{
			CompressionMethod: CompressionMethod(compressionMethod),
			Name:              filename,
			Comment:           comment,
			EncryptionMethod:  encryptionMethod,
		},
	}
	f.openFunc = func() (io.ReadCloser, error) {
		return zr.openFile(f)
	}
	return f
}

// openFile implements the openFunc for a File created from central directory.
// It reads the local file header, handles decryption if needed, and sets up
// decompression with CRC32 verification
func (zr *zipReader) openFile(f *File) (io.ReadCloser, error) {
	buf := make([]byte, localHeaderLen)

	var headerReader io.Reader
	// Use ReaderAt interface for efficient random access if available
	if ra, ok := zr.src.(io.ReaderAt); ok {
		headerReader = io.NewSectionReader(ra, f.localHeaderOffset, localHeaderLen)
	} else {
		if _, err := zr.src.Seek(f.localHeaderOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to local header: %w", err)
		}
		headerReader = zr.src
	}

	if _, err := io.ReadFull(headerReader, buf); err != nil {
		return nil, fmt.Errorf("read local header: %w", err)
	}

	if binary.LittleEndian.Uint32(buf[0:4]) != __LOCAL_FILE_HEADER_SIGNATURE {
		return nil, errors.New("invalid local file header signature")
	}

	bitFlag := binary.LittleEndian.Uint16(buf[6:8])
	isEncrypted := bitFlag&0x1 != 0

	filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))
	dataOffset := f.localHeaderOffset + localHeaderLen + filenameLen + extraLen

	var dataR io.Reader
	// Create reader for the compressed/encrypted file data
	if ra, ok := zr.src.(io.ReaderAt); ok {
		dataR = io.NewSectionReader(ra, dataOffset, f.compressedSize)
	} else {
		if _, err := zr.src.Seek(dataOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to file data: %w", err)
		}
		dataR = io.LimitReader(zr.src, f.compressedSize)
	}

	if isEncrypted {
		if f.config.Password == "" {
			return nil, errors.New("file is encrypted but no password provided")
		}

		cryptoR, err := zr.createDecrypter(dataR, f.config, f.crc32)
		if err != nil {
			return nil, err
		}
		dataR = cryptoR
	}

	zr.mu.RLock()
	defer zr.mu.RUnlock()
	decompressor, ok := zr.decompressors[f.config.CompressionMethod]
	if !ok {
		return nil, fmt.Errorf("unsupported compression method: %d", f.config.CompressionMethod)
	}

	rc, err := decompressor.Decompress(dataR)
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

// createDecrypter instantiates the appropriate decryption reader based on encryption method.
// ZipCrypto uses CRC32 MSB for password verification, AES uses salt-based verification.
func (zr *zipReader) createDecrypter(src io.Reader, cfg FileConfig, CRC32 uint32) (io.Reader, error) {
	switch cfg.EncryptionMethod {
	case ZipCrypto:
		return NewZipCryptoReader(src, cfg.Password, CRC32)
	case AES256:
		return NewAes256Reader(src, cfg.Password)
	default:
		return nil, fmt.Errorf("unknown encryption method: %d", cfg.EncryptionMethod)
	}
}

// verifySignature checks whether the next 4 bytes in the source match the given signature
func (zr *zipReader) verifySignature(s uint32) bool {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(zr.src, buf); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(buf) == s
}

// parseExtraField converts raw extra field bytes into a map keyed by tag IDs.
// Extra fields contain additional metadata like ZIP64 extensions, NTFS timestamps, etc.
func parseExtraField(extraField []byte) map[uint16][]byte {
	m := make(map[uint16][]byte)

	for offset := 0; offset < len(extraField); {
		// Need at least 4 bytes for tag and size
		if offset+4 > len(extraField) {
			break
		}

		tag := binary.LittleEndian.Uint16(extraField[offset : offset+2])
		size := int(binary.LittleEndian.Uint16(extraField[offset+2 : offset+4]))

		offset += 4

		if offset+size > len(extraField) {
			break
		}

		// Store the entire extra field block (header + data)
		m[tag] = extraField[offset-4 : offset+size]
		offset += size
	}
	return m
}

// parseFileExternalAttributes converts file attributes from central directory to fs.FileMode
func parseFileExternalAttributes(entry centralDirectory) fs.FileMode {
	var mode fs.FileMode
	hostSystem := HostSystem(entry.VersionMadeBy >> 8)

	if hostSystem == HostSystemUNIX {
		unixMode := uint32(entry.ExternalFileAttributes >> 16)
		mode = fs.FileMode(unixMode & 0777)

		switch {
		case unixMode&s_IFDIR != 0:
			mode |= fs.ModeDir
		case unixMode&s_IFLNK != 0:
			mode |= fs.ModeSymlink
		}
	} else {
		if strings.HasSuffix(entry.Filename, "/") {
			mode = 0755 | fs.ModeDir
		} else {
			mode = 0644
		}
		if entry.ExternalFileAttributes&0x01 != 0 {
			mode &^= 0222 // Remove write permission (a-w)
		}
	}
	return mode
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
			return n, errors.New("file is larger than declared in header")
		}
		cr.hash.Write(p[:n])
	}
	return n, err
}

// Close implements io.Closer interface and verifies CRC32 and size after reading completes
func (cr *checksumReader) Close() error {
	defer cr.rc.Close()

	if cr.read != cr.size {
		return fmt.Errorf("size mismatch: read %d, want %d", cr.read, cr.size)
	}

	if got := cr.hash.Sum32(); got != cr.want {
		return fmt.Errorf("checksum mismatch: got %x, want %x", got, cr.want)
	}
	return nil
}
