package gozip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
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
			mw := NewMemoryWriteSeeker()
			z := NewZip()
			zw := newZipWriter(z, mw)

			err := zw.writeFileHeader(tt.file)

			if (err != nil) != tt.wantErr {
				t.Errorf("WriteFileHeader() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify header signature
			buf := mw.Bytes()
			if len(buf) < 4 {
				t.Error("WriteFileHeader() should write at least 4 bytes")
			}
			signature := binary.LittleEndian.Uint32(buf[:4])
			if signature != __LOCAL_FILE_HEADER_SIGNATURE {
				t.Errorf("WriteFileHeader() signature = %x, want %x",
					signature, __LOCAL_FILE_HEADER_SIGNATURE)
			}
		})
	}
}

// TestZipWriter_EncodeToWriter tests the decoupled compression logic
func TestZipWriter_EncodeToWriter(t *testing.T) {
	testData := "This is a long text with some repetition to demonstrate compression in action"

	tests := []struct {
		name        string
		compression CompressionMethod
		level       int
		wantErr     bool
	}{
		{
			name:        "store compression",
			compression: Stored,
			wantErr:     false,
		},
		{
			name:        "deflate compression",
			compression: Deflated,
			level:       DeflateNormal,
			wantErr:     false,
		},
		{
			name:        "unsupported compression",
			compression: CompressionMethod(99), // Invalid method
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var destBuf bytes.Buffer
			// Here we rely on resolveCompressor logic inside encodeToWriter
			z := NewZip()
			zw := newZipWriter(z, NewMemoryWriteSeeker())

			src := strings.NewReader(testData)
			config := FileConfig{
				CompressionMethod: tt.compression,
				CompressionLevel:  tt.level,
			}

			stats, err := zw.encodeToWriter(src, &destBuf, config)

			if (err != nil) != tt.wantErr {
				t.Errorf("encodeToWriter() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return // Expected error case
			}

			// Verify Stats
			if stats.uncompressedSize != int64(len(testData)) {
				t.Errorf("Stats.uncompressed = %d, want %d", stats.uncompressedSize, len(testData))
			}
			if stats.compressedSize == 0 {
				t.Error("Stats.compressed should be > 0")
			}

			expectedCRC := crc32.ChecksumIEEE([]byte(testData))
			if stats.crc32 != expectedCRC {
				t.Errorf("Stats.crc32 = %x, want %x", stats.crc32, expectedCRC)
			}

			// Verify Compression Ratio logic
			if tt.compression == Stored && stats.compressedSize != stats.uncompressedSize {
				t.Errorf("Stored: compressed (%d) != uncompressed (%d)", stats.compressedSize, stats.uncompressedSize)
			}
			if tt.compression == Deflated && stats.compressedSize >= stats.uncompressedSize {
				// Note: For very small strings deflate might be larger, but for this string it should be smaller
				t.Logf("Warning: Deflate did not compress data (size %d -> %d)", stats.uncompressedSize, stats.compressedSize)
			}
		})
	}
}

// TestZipWriter_WriteFile_Strategies tests both Stream (known size) and Buffered (unknown size) paths
func TestZipWriter_WriteFile_Strategies(t *testing.T) {
	data := []byte("test data for strategies")

	tests := []struct {
		name             string
		uncompressedSize int64 // 0 triggers buffered path, >0 triggers stream path
		isDir            bool
	}{
		{
			name:             "Stream Path (Known Size)",
			uncompressedSize: int64(len(data)),
			isDir:            false,
		},
		{
			name:             "Buffered Path (Unknown Size)",
			uncompressedSize: 0,
			isDir:            false,
		},
		{
			name:             "Directory Path",
			uncompressedSize: 0,
			isDir:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := NewMemoryWriteSeeker()
			z := NewZip()
			z.SetConfig(ZipConfig{CompressionMethod: Stored}) // Use Store for simplicity
			zw := newZipWriter(z, mw)

			file := &file{
				name:             "test",
				uncompressedSize: tt.uncompressedSize,
				isDir:            tt.isDir,
				modTime:          defaultTime(),
				config:           FileConfig{CompressionMethod: Stored},
				// OpenFunc that simulates source
				openFunc: func() (io.ReadCloser, error) {
					if tt.isDir {
						return nil, nil
					}
					return io.NopCloser(bytes.NewReader(data)), nil
				},
			}

			err := zw.WriteFile(file)
			if err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			// Basic verification
			output := mw.Bytes()
			if len(output) == 0 {
				t.Fatal("Output is empty")
			}

			// Verify file struct was updated
			if !tt.isDir {
				if file.crc32 == 0 {
					t.Error("File CRC32 was not updated")
				}
				if file.compressedSize == 0 {
					t.Error("File compressed size was not updated")
				}
			}
		})
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
		modTime:           defaultTime(),
	}

	mw := NewMemoryWriteSeeker()
	zw := newZipWriter(NewZip(), mw)

	// 1. Write initial header
	err := zw.writeFileHeader(file)
	if err != nil {
		t.Fatalf("WriteFileHeader() error = %v", err)
	}

	// 2. Simulate data writing (move cursor)
	mw.Write(make([]byte, 100))

	// 3. Update with compression results
	err = zw.updateLocalHeader(file)
	if err != nil {
		t.Errorf("UpdateLocalHeader() error = %v", err)
	}

	// Verify the header was updated by reading back the CRC and sizes
	headerData := mw.Bytes()
	if len(headerData) < 30 {
		t.Error("Header data too short")
	}

	// Read CRC32 from header (offset 14)
	crcFromHeader := binary.LittleEndian.Uint32(headerData[14:18])
	if crcFromHeader != file.crc32 {
		t.Errorf("CRC32 in header = %x, want %x", crcFromHeader, file.crc32)
	}
}

// TestZipWriter_Integration tests complete zip creation flow via public API
func TestZipWriter_Integration(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	z := NewZip()
	z.SetConfig(ZipConfig{CompressionMethod: Deflated})
	zw := newZipWriter(z, mw)

	content := "Integration test data"
	file := &file{
		name:     "integration_test.txt",
		openFunc: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil },
		modTime:  defaultTime(),
		// Important: Set uncompressedSize to content length to trigger Stream path,
		// or 0 to trigger Buffered path. Testing Stream path here.
		uncompressedSize: int64(len(content)),
		config:           FileConfig{CompressionMethod: Deflated},
	}

	// Use the main public method
	err := zw.WriteFile(file)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Finish archive
	err = zw.WriteCentralDirAndEndRecords()
	if err != nil {
		t.Fatalf("WriteCentralDirAndEndRecords failed: %v", err)
	}

	// Verify final archive is not empty and has signatures
	output := mw.Bytes()
	if len(output) == 0 {
		t.Error("Integration test produced empty archive")
	}

	// Check for Central Directory Signature at the end area
	if !bytes.Contains(output, []byte{0x50, 0x4b, 0x01, 0x02}) {
		t.Error("Central Directory signature not found")
	}
}

func TestParallelZipWriter_Basic(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	z := NewZip()
	// Use Deflate to ensure actual work is done in parallel
	z.SetConfig(ZipConfig{CompressionMethod: Deflated})

	// Create multiple files
	filesCount := 5
	files := make([]*file, filesCount)
	content := "Parallel test data content"

	for i := 0; i < filesCount; i++ {
		name := fmt.Sprintf("file_%d.txt", i)
		files[i] = &file{
			name:             name,
			uncompressedSize: int64(len(content)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Deflated, CompressionLevel: DeflateNormal},
			openFunc: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(content)), nil
			},
		}
	}

	// Initialize parallel writer with 2 workers
	pzw := newParallelZipWriter(z, mw, 2)

	// Run WriteFiles
	errs := pzw.WriteFiles(files)
	if len(errs) > 0 {
		t.Fatalf("WriteFiles returned errors: %v", errs)
	}

	// Finish archive (write Central Directory)
	// Note: pzw.zw is the underlying sequential writer
	if err := pzw.zw.WriteCentralDirAndEndRecords(); err != nil {
		t.Fatalf("WriteCentralDirAndEndRecords failed: %v", err)
	}

	output := mw.Bytes()

	// Check integrity
	if len(output) == 0 {
		t.Fatal("Output archive is empty")
	}

	// Check existence of filenames in output (simple check)
	// Note: We can't check order because ParallelWriter completion order is non-deterministic,
	// but the WriteFiles implementation usually writes them in completion order.
	// The underlying implementation loop: `for result := range results`.
	// This implies the physical order in ZIP depends on which file compresses first.
	for i := 0; i < filesCount; i++ {
		name := fmt.Sprintf("file_%d.txt", i)
		if !bytes.Contains(output, []byte(name)) {
			t.Errorf("Output archive missing file: %s", name)
		}
	}

	// Check End of Central Directory signature
	if !bytes.Contains(output, []byte{0x50, 0x4b, 0x05, 0x06}) {
		t.Error("End of Central Directory signature not found")
	}
}

func TestParallelZipWriter_MemoryVsDisk(t *testing.T) {
	// This test manipulates the memoryThreshold to force both paths:
	// 1. In-memory buffer
	// 2. Temporary file on disk
	
	mw := NewMemoryWriteSeeker()
	z := NewZip()
	z.SetConfig(ZipConfig{CompressionMethod: Stored})

	smallContent := "small"      // 5 bytes
	largeContent := "larger_data" // 11 bytes

	files := []*file{
		{
			name:             "memory_file.txt",
			uncompressedSize: int64(len(smallContent)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Stored},
			openFunc:         func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(smallContent)), nil },
		},
		{
			name:             "disk_file.txt",
			uncompressedSize: int64(len(largeContent)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Stored},
			openFunc:         func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(largeContent)), nil },
		},
	}

	pzw := newParallelZipWriter(z, mw, 1)
	
	// HACK: Manually set threshold low to force the second file to disk
	// Accessing private field since we are in the same package
	pzw.memoryThreshold = 10 // bytes

	errs := pzw.WriteFiles(files)
	if len(errs) > 0 {
		t.Fatalf("WriteFiles errors: %v", errs)
	}

	if err := pzw.zw.WriteCentralDirAndEndRecords(); err != nil {
		t.Fatal(err)
	}

	output := mw.Bytes()

	// Verify contents
	if !bytes.Contains(output, []byte(smallContent)) {
		t.Error("Small file content missing")
	}
	if !bytes.Contains(output, []byte(largeContent)) {
		t.Error("Large file content missing")
	}

	// Verify stats update
	if files[1].compressedSize != int64(len(largeContent)) {
		t.Errorf("Large file compressed size mismatch: got %d", files[1].compressedSize)
	}
}

func TestParallelZipWriter_ErrorHandling(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	z := NewZip()
	pzw := newParallelZipWriter(z, mw, 2)

	expectedErr := errors.New("simulated open error")
	files := []*file{
		{
			name: "bad_file.txt",
			openFunc: func() (io.ReadCloser, error) {
				return nil, expectedErr
			},
		},
	}

	errs := pzw.WriteFiles(files)
	if len(errs) == 0 {
		t.Fatal("Expected error, got none")
	}

	// Check if the error is wrapped or passed through
	found := false
	for _, err := range errs {
		if strings.Contains(err.Error(), expectedErr.Error()) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected error containing %q, got %v", expectedErr, errs)
	}
}

// memoryWriteSeeker implements io.WriteSeeker correctly for testing
type memoryWriteSeeker struct {
	buf []byte
	pos int64
}

func NewMemoryWriteSeeker() *memoryWriteSeeker {
	return &memoryWriteSeeker{
		buf: make([]byte, 0),
		pos: 0,
	}
}

func (m *memoryWriteSeeker) Write(p []byte) (n int, err error) {
	minCap := int(m.pos) + len(p)
	if minCap > cap(m.buf) {
		newBuf := make([]byte, len(m.buf), minCap*2) // Grow strategy
		copy(newBuf, m.buf)
		m.buf = newBuf
	}

	// Extend length if writing past current len
	if minCap > len(m.buf) {
		m.buf = m.buf[:minCap]
	}

	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memoryWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = m.pos + offset
	case io.SeekEnd:
		newPos = int64(len(m.buf)) + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if newPos < 0 {
		return 0, errors.New("negative position")
	}

	m.pos = newPos
	return newPos, nil
}

func (m *memoryWriteSeeker) Bytes() []byte {
	return m.buf
}

func defaultTime() time.Time {
	return time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
}
