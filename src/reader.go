package gozip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"strings"
	"sync"
)

const (
	localHeaderLen  = 30 // Size of local header without extensible fields
	directoryEndLen = 22 // Size of EOCD without comment
	zip64LocatorLen = 20 // Size of Zip64 Locator
)

type zipReader struct {
	mu            sync.RWMutex
	src           io.ReadSeeker                      // Target stream for reading archive data
	decompressors map[CompressionMethod]Decompressor // Reusable decompressors map
}

func newZipReader(src io.ReadSeeker, decompressors map[CompressionMethod]Decompressor) *zipReader {
	if decompressors == nil {
		decompressors = make(map[CompressionMethod]Decompressor)
	}
	decompressors[Stored] = new(StoredDecompressor)
	decompressors[Deflated] = new(DeflateDecompressor)

	return &zipReader{
		src: src,
		decompressors: decompressors,
	}
}

// ReadFiles reads source and returns list of files stored in the archive
func (zr *zipReader) ReadFiles() ([]*file, error) {
	endDir, err := zr.readEndOfCentralDir()
	if err != nil {
		return nil, err
	}

	centralDirOffset := int64(endDir.CentralDirOffset)
	entriesNum := int64(endDir.TotalNumberOfEntries)

	// Check for zip64 records
	if centralDirOffset == math.MaxUint32 || entriesNum == math.MaxUint16 {
		fileSize, _ := zr.src.Seek(0, io.SeekEnd)

		zip64locatorOffset := fileSize - int64(directoryEndLen+endDir.CommentLength) - int64(zip64LocatorLen)
		if _, err := zr.src.Seek(zip64locatorOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to zip64 locator: %w", err)
		}
		if !zr.verifySignature(__ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE) {
			return nil, errors.New("expected zip64 end of central directory locator signature")
		}

		zip64Locator, err := readZip64EndOfCentralDirLocator(zr.src)
		if err != nil {
			return nil, fmt.Errorf("read zip64 end of central dir locator: %w", err)
		}

		zip64EndDirOffset := int64(zip64Locator.Zip64EndOfCentralDirOffset)
		if _, err := zr.src.Seek(zip64EndDirOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to zip64 eocd: %w", err)
		}
		if !zr.verifySignature(__ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE) {
			return nil, errors.New("expected zip64 end of central directory signature")
		}

		zip64EndDir, err := readZip64EndOfCentralDir(zr.src)
		if err != nil {
			return nil, fmt.Errorf("read zip64 end of central directory: %w", err)
		}

		centralDirOffset = int64(zip64EndDir.CentralDirOffset)
		entriesNum = int64(zip64EndDir.TotalNumberOfEntries)
	}

	return zr.readCentralDir(centralDirOffset, entriesNum)
}

// readEndOfCentralDir scans for the end of central directory record and reads it
func (zr *zipReader) readEndOfCentralDir() (endOfCentralDirectory, error) {
	fileSize, _ := zr.src.Seek(0, io.SeekEnd)
	if fileSize < directoryEndLen {
		return endOfCentralDirectory{}, errors.New("file too small")
	}

	const bufSize = 1024
	buf := make([]byte, bufSize)
	rangeLimit := min(int64(math.MaxUint16+directoryEndLen), fileSize)

	for i := int64(0); i < rangeLimit; {
		readSize := int64(bufSize)
		if i+readSize > rangeLimit {
			readSize = rangeLimit - i
		}

		offset := max(fileSize - directoryEndLen - i - readSize + int64(directoryEndLen), 0)
		if _, err := zr.src.Seek(offset, io.SeekStart); err != nil {
			return endOfCentralDirectory{}, err
		}

		n, _ := io.ReadFull(zr.src, buf[:readSize])

		for p := n - 4; p >= 0; p-- {
			if binary.LittleEndian.Uint32(buf[p:p+4]) == __END_OF_CENTRAL_DIRECTORY_SIGNATURE {
				zr.src.Seek(offset+int64(p)+4, io.SeekStart)
				return readEndOfCentralDir(zr.src)
			}
		}

		i += readSize - 3
	}

	return endOfCentralDirectory{}, errors.New("couldn't locate end of central dir signature")
}

// readCentralDir reads central directory entries and parses them into files
func (zr *zipReader) readCentralDir(offset, entries int64) ([]*file, error) {
	files := make([]*file, 0, entries)

	if _, err := zr.src.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to central dir offset: %w", err)
	}

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

// newFileFromCentralDir creates a file from central directory entry
func (zr *zipReader) newFileFromCentralDir(entry centralDirectory) *file {
	var isDir bool
	filename := entry.Filename
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

	f := &file{
		name:              filename,
		isDir:             isDir,
		uncompressedSize:  int64(uncompressedSize),
		compressedSize:    int64(compressedSize),
		crc32:             entry.CRC32,
		localHeaderOffset: int64(localHeaderOffset),
		hostSystem:        HostSystem(entry.VersionMadeBy >> 8),
		modTime:           msDosToTime(entry.LastModFileDate, entry.LastModFileTime),
		extraField:        entry.ExtraField,
		config: FileConfig{
			CompressionMethod: CompressionMethod(entry.CompressionMethod),
			Name:              filename,
			Comment:           entry.Comment,
		},
	}
	f.openFunc = func() (io.ReadCloser, error) {
		return zr.openFile(f)
	}
	return f
}

// openFile implements openFunc for file made from central directory
func (zr *zipReader) openFile(f *file) (io.ReadCloser, error) {
	buf := make([]byte, localHeaderLen)

	var headerReader io.Reader
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

	filenameLen := int64(binary.LittleEndian.Uint16(buf[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(buf[28:30]))

	dataOffset := f.localHeaderOffset + localHeaderLen + filenameLen + extraLen

	var dataR io.Reader
	if ra, ok := zr.src.(io.ReaderAt); ok {
		dataR = io.NewSectionReader(ra, dataOffset, f.compressedSize)
	} else {
		if _, err := zr.src.Seek(dataOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to file data: %w", err)
		}
		dataR = io.LimitReader(zr.src, f.compressedSize)
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

// verifySignatures checks whether next 4 bytes match given signature by reading from source
func (zr *zipReader) verifySignature(s uint32) bool {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(zr.src, buf); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(buf) == s
}

// parseExtraField turns extra field bytes into a map with tags as keys and raw data as values
func parseExtraField(extraField []byte) map[uint16][]byte {
	m := make(map[uint16][]byte)

	for offset := 0; offset < len(extraField); {
		if offset+4 > len(extraField) {
			break
		}
		tag := binary.LittleEndian.Uint16(extraField[offset : offset+2])
		size := int(binary.LittleEndian.Uint16(extraField[offset+2 : offset+4]))

		offset += 4
		if offset+size > len(extraField) {
			break
		}

		m[tag] = extraField[offset : offset+size]
		offset += size
	}
	return m
}

// checksumReader wraps io.ReadCloser for checksum verification
type checksumReader struct {
	rc   io.ReadCloser
	hash hash.Hash32
	want uint32
	read uint64
	size uint64
}

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
