package gozip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestZipWriter_WriteFileHeader tests local file header writing
func TestZipWriter_WriteFileHeader(t *testing.T) {
	tests := []struct {
		name    string
		file    *file
		wantErr bool
	}{
		{
			name: "basic file header",
			file: &file{
				name:    "test.txt",
				modTime: defaultTime(),
			},
			wantErr: false,
		},
		{
			name: "file with long name",
			file: &file{
				name:    strings.Repeat("a", 100) + ".txt",
				modTime: defaultTime(),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := newZipWriter(NewZip(Stored), &seekableBuffer{Buffer: &buf})

			err := zw.writeFileHeader(tt.file)

			if (err != nil) != tt.wantErr {
				t.Errorf("WriteFileHeader() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify header signature
			if buf.Len() < 4 {
				t.Error("WriteFileHeader() should write at least 4 bytes")
			}
			signature := binary.LittleEndian.Uint32(buf.Bytes()[:4])
			if signature != __LOCAL_FILE_HEADER_SIGNATURE {
				t.Errorf("WriteFileHeader() signature = %x, want %x",
					signature, __LOCAL_FILE_HEADER_SIGNATURE)
			}
		})
	}
}

// TestZipWriter_CompressFileData tests file data compression with different methods
func TestZipWriter_CompressFileData(t *testing.T) {
	testData := "This is a long text with some repetition to demonstrate compression in action"

	tests := []struct {
		name        string
		compression CompressionMethod
		source      io.Reader
		wantErr     bool
	}{
		{
			name:        "store compression",
			compression: Stored,
			source:      strings.NewReader(testData),
			wantErr:     false,
		},
		{
			name:        "deflate compression",
			compression: Deflated,
			source:      strings.NewReader(testData),
			wantErr:     false,
		},
		{
			name:        "unsupported compression",
			compression: CompressionMethod(99), // Invalid method
			source:      strings.NewReader(testData),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := newZipWriter(NewZip(tt.compression), &seekableBuffer{Buffer: &buf})

			file := &file{
				name:   "test.txt",
				source: tt.source,
				config: FileConfig{CompressionMethod: tt.compression},
			}

			tmpFile, err := zw.encodeFileData(file)

			// Clean up temp file if created
			if tmpFile != nil {
				defer tmpFile.Close()
				defer os.Remove(tmpFile.Name())
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("CompressFileData() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return // Expected error case
			}

			// Verify file metadata was updated
			if file.uncompressedSize == 0 {
				t.Error("CompressFileData() should set uncompressedSize")
			}
			if file.compressedSize == 0 {
				t.Error("CompressFileData() should set compressedSize")
			}
			if file.crc32 == 0 {
				t.Error("CompressFileData() should set crc32")
			}

			// Verify CRC32 matches expected
			expectedCRC := crc32.ChecksumIEEE([]byte(testData))
			if file.crc32 != expectedCRC {
				t.Errorf("CompressFileData() crc32 = %x, want %x",
					file.crc32, expectedCRC)
			}

			// For stored compression, sizes should be equal
			if tt.compression == Stored && file.compressedSize != file.uncompressedSize {
				t.Errorf("Stored compression: compressedSize = %d, uncompressedSize = %d, should be equal",
					file.compressedSize, file.uncompressedSize)
			}

			// For deflate compression, compressed size should be <= uncompressed
			if tt.compression == Deflated && file.compressedSize > file.uncompressedSize {
				t.Errorf("Deflated compression: compressedSize = %d > uncompressedSize = %d",
					file.compressedSize, file.uncompressedSize)
			}
		})
	}
}

// TestZipWriter_AddCentralDirEntry tests central directory entry creation
func TestZipWriter_AddCentralDirEntry(t *testing.T) {
	file := &file{
		name:              "test.txt",
		modTime:           defaultTime(),
		config:            FileConfig{CompressionMethod: Stored},
		localHeaderOffset: 0,
	}

	var buf bytes.Buffer
	zw := newZipWriter(NewZip(Stored), &seekableBuffer{Buffer: &buf})

	err := zw.addCentralDirEntry(file)
	if err != nil {
		t.Errorf("AddCentralDirEntry() error = %v", err)
	}

	if zw.sizeOfCentralDir == 0 {
		t.Error("AddCentralDirEntry() should update sizeOfCentralDir")
	}

	if zw.centralDirBuf.Len() == 0 {
		t.Error("AddCentralDirEntry() should write to centralDirBuf")
	}

	// Verify central directory signature
	cdData := zw.centralDirBuf.Bytes()
	if len(cdData) < 4 {
		t.Error("Central directory entry too short")
	}
	signature := binary.LittleEndian.Uint32(cdData[:4])
	if signature != __CENTRAL_DIRECTORY_SIGNATURE {
		t.Errorf("Central directory signature = %x, want %x",
			signature, __CENTRAL_DIRECTORY_SIGNATURE)
	}
}

// TestZipWriter_UpdateLocalHeader tests local header updates after compression
func TestZipWriter_UpdateLocalHeader(t *testing.T) {
	file := &file{
		name:              "test.txt",
		crc32:             0x12345678,
		compressedSize:    100,
		uncompressedSize:  100,
		localHeaderOffset: 0,
	}

	var buf bytes.Buffer
	zw := newZipWriter(NewZip(Stored), &seekableBuffer{Buffer: &buf})

	// Write initial header
	err := zw.writeFileHeader(file)
	if err != nil {
		t.Fatalf("WriteFileHeader() error = %v", err)
	}

	// Update with compression results
	err = zw.updateLocalHeader(file)
	if err != nil {
		t.Errorf("UpdateLocalHeader() error = %v", err)
	}

	// Verify the header was updated by reading back the CRC and sizes
	headerData := buf.Bytes()
	if len(headerData) < 30 {
		t.Error("Header data too short")
	}

	// Read CRC32 from header (offset 14)
	crcFromHeader := binary.LittleEndian.Uint32(headerData[14:18])
	if crcFromHeader != file.crc32 {
		t.Errorf("CRC32 in header = %x, want %x", crcFromHeader, file.crc32)
	}

	// Read compressed size from header (offset 18)
	compressedSizeFromHeader := binary.LittleEndian.Uint32(headerData[18:22])
	if compressedSizeFromHeader != uint32(file.compressedSize) {
		t.Errorf("Compressed size in header = %d, want %d",
			compressedSizeFromHeader, file.compressedSize)
	}

	// Read uncompressed size from header (offset 22)
	uncompressedSizeFromHeader := binary.LittleEndian.Uint32(headerData[22:26])
	if uncompressedSizeFromHeader != uint32(file.uncompressedSize) {
		t.Errorf("Uncompressed size in header = %d, want %d",
			uncompressedSizeFromHeader, file.uncompressedSize)
	}
}

// TestZipWriter_WriteFileData tests copying from temporary file
func TestZipWriter_WriteFileData(t *testing.T) {
	var destBuf bytes.Buffer
	zw := newZipWriter(NewZip(Stored), &seekableBuffer{Buffer: &destBuf})

	// Create temporary file with test data
	testData := "Temporary file test data"
	tmpFile, err := os.CreateTemp("", "zip-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_, err = tmpFile.WriteString(testData)
	if err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Seek(0, io.SeekStart)

	// Write temp file data to destination
	err = zw.writeFileData(tmpFile)
	if err != nil {
		t.Errorf("WriteFileData() error = %v", err)
	}

	// Verify data was copied
	if destBuf.String() != testData {
		t.Errorf("WriteFileData() copied data = %q, want %q",
			destBuf.String(), testData)
	}
}

// TestZipWriter_Integration tests complete zip creation flow
func TestZipWriter_Integration(t *testing.T) {
	var buf bytes.Buffer
	zw := newZipWriter(NewZip(Deflated), &seekableBuffer{Buffer: &buf})

	// Create test file
	file := &file{
		name:    "integration_test.txt",
		source:  strings.NewReader("Integration test data"),
		modTime: defaultTime(),
		config:  FileConfig{CompressionMethod: Deflated},
	}

	// Complete flow
	err := zw.writeFileHeader(file)
	if err != nil {
		t.Fatalf("WriteFileHeader failed: %v", err)
	}

	tmpFile, err := zw.encodeFileData(file)
	if err != nil {
		t.Fatalf("CompressFileData failed: %v", err)
	}
	if tmpFile != nil {
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()
	}

	err = zw.writeFileData(tmpFile)
	if err != nil {
		t.Fatalf("WriteFileData failed: %v", err)
	}

	err = zw.updateLocalHeader(file)
	if err != nil {
		t.Fatalf("UpdateLocalHeader failed: %v", err)
	}

	err = zw.addCentralDirEntry(file)
	if err != nil {
		t.Fatalf("AddCentralDirEntry failed: %v", err)
	}

	err = zw.WriteCentralDirAndEndRecords()
	if err != nil {
		t.Fatalf("WriteCentralDirAndEndDir failed: %v", err)
	}

	// Verify final archive is not empty
	if buf.Len() == 0 {
		t.Error("Integration test produced empty archive")
	}
}

// Helper types and functions

// seekableBuffer implements io.WriteSeeker for bytes.Buffer
type seekableBuffer struct {
	*bytes.Buffer
	pos int64
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	case io.SeekEnd:
		s.pos = int64(s.Buffer.Len()) + offset
	}

	if s.pos < 0 {
		return 0, io.ErrUnexpectedEOF
	}

	return s.pos, nil
}

func (s *seekableBuffer) Write(p []byte) (n int, err error) {
	// Grow buffer if needed
	if int64(s.Buffer.Len()) < s.pos+int64(len(p)) {
		s.Buffer.Grow(int(s.pos) + len(p) - s.Buffer.Len())
	}

	// Write to current position
	n = copy(s.Buffer.Bytes()[s.pos:], p)
	s.pos += int64(n)

	if n < len(p) {
		// Append remaining data
		n2, _ := s.Buffer.Write(p[n:])
		s.pos += int64(n2)
		n += n2
	}

	return n, nil
}

func defaultTime() time.Time {
	return time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
}
