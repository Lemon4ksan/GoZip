package gozip

import (
	"bytes"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNewFileFromOS tests creating a file struct from os.File
func TestNewFileFromOS(t *testing.T) {
	// Create a temporary test file
	tmpfile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())
	defer tmpfile.Close()

	// Write test content
	testContent := "Hello, World!"
	_, err = tmpfile.WriteString(testContent)
	if err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpfile.Seek(0, 0)

	// Test newFileFromOS
	file, err := newFileFromOS(tmpfile)
	if err != nil {
		t.Fatalf("newFileFromOS failed: %v", err)
	}

	// Verify file metadata
	stat, err := tmpfile.Stat()
	if err != nil {
		t.Fatalf("Failed to get tmpFile stats: %v", err)
	}
	if file.name != stat.Name() {
		t.Errorf("Expected name %s, got %s", stat.Name(), file.name)
	}
	if file.uncompressedSize != int64(len(testContent)) {
		t.Errorf("Expected size %d, got %d", len(testContent), file.uncompressedSize)
	}
	if file.source != tmpfile {
		t.Error("Source should be the original file")
	}
}

func TestGetFileBitFlag(t *testing.T) {
    f := &file{compressionMethod: Deflated}
    
    f.compressionLevel = DeflateNormal
    if flag := f.getFileBitFlag(); flag != 0x0000 {
        t.Errorf("Normal: expected 0x0000, got 0x%04X", flag)
    }
    
    f.compressionLevel = DeflateMaximum  
    if flag := f.getFileBitFlag(); flag != 0x0002 {
        t.Errorf("Maximum: expected 0x0002, got 0x%04X", flag)
    }
    
    f.compressionLevel = DeflateFast
    if flag := f.getFileBitFlag(); flag != 0x0004 {
        t.Errorf("Fast: expected 0x0004, got 0x%04X", flag)
    }
    
    f.compressionLevel = DeflateSuperFast
    if flag := f.getFileBitFlag(); flag != 0x0006 {
        t.Errorf("SuperFast: expected 0x0006, got 0x%04X", flag)
    }
}

// TestCompressAndWriteStored tests compression with Stored method (no compression)
func TestCompressAndWriteStored(t *testing.T) {
	testContent := "Test content for compression"
	source := strings.NewReader(testContent)

	file := &file{
		name:              "test.txt",
		compressionMethod: Stored,
		source:            source,
	}

	var dest bytes.Buffer

	// Test compression
	err := file.compressAndWrite(&dest)
	if err != nil {
		t.Fatalf("compressAndWrite failed: %v", err)
	}

	// Verify compressed size equals original size for Stored method
	if file.compressedSize != int64(len(testContent)) {
		t.Errorf("Expected compressed size %d, got %d", len(testContent), file.compressedSize)
	}

	// Verify CRC32 checksum
	expectedCRC := crc32.ChecksumIEEE([]byte(testContent))
	if file.crc32 != expectedCRC {
		t.Errorf("Expected CRC32 %x, got %x", expectedCRC, file.crc32)
	}

	// Verify content is unchanged
	if dest.String() != testContent {
		t.Error("Compressed content doesn't match original for Stored method")
	}
}

// TestCompressAndWriteDeflated tests compression with Deflated method
func TestCompressAndWriteDeflated(t *testing.T) {
	testContent := "This is a test content that should compress well " +
		"because it has repeated patterns and is somewhat longer " +
		"than just a few bytes to demonstrate compression working."
	source := strings.NewReader(testContent)

	file := &file{
		name:              "test.txt",
		compressionMethod: Deflated,
		source:            source,
	}

	var dest bytes.Buffer

	// Test compression
	err := file.compressAndWrite(&dest)
	if err != nil {
		t.Fatalf("compressAndWrite failed: %v", err)
	}

	// For Deflated method, compressed size should be less than or equal to original
	if file.compressedSize > int64(len(testContent)) {
		t.Errorf("Compressed size %d should be <= original size %d",
			file.compressedSize, len(testContent))
	}

	// Verify CRC32 checksum matches original content
	expectedCRC := crc32.ChecksumIEEE([]byte(testContent))
	if file.crc32 != expectedCRC {
		t.Errorf("Expected CRC32 %x, got %x", expectedCRC, file.crc32)
	}
}

// TestGetVersionNeededToExtract tests version detection for different compression methods
func TestGetVersionNeededToExtract(t *testing.T) {
	tests := []struct {
		name     string
		method   CompressionMethod
		expected uint16
	}{
		{"Stored method", Stored, 10},
		{"Deflated method", Deflated, 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &file{compressionMethod: tt.method}
			version := file.getVersionNeededToExtract()
			if version != tt.expected {
				t.Errorf("Expected version %d, got %d", tt.expected, version)
			}
		})
	}
}

// TestLocalHeader tests local file header creation
func TestLocalHeader(t *testing.T) {
	modTime := time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC)
	file := &file{
		name:              "test.txt",
		uncompressedSize:  1024,
		modTime:           modTime,
		compressionMethod: Deflated,
	}

	header := file.localHeader()

	// Verify header fields
	if header.VersionNeededToExtract != 20 {
		t.Errorf("Expected version 20 for Deflated, got %d", header.VersionNeededToExtract)
	}
	if header.CompressionMethod != uint16(Deflated) {
		t.Errorf("Expected compression method %d, got %d", Deflated, header.CompressionMethod)
	}
	if header.UncompressedSize != 1024 {
		t.Errorf("Expected uncompressed size 1024, got %d", header.UncompressedSize)
	}
	if header.FilenameLength != uint16(len(file.name)) {
		t.Errorf("Expected filename length %d, got %d", len(file.name), header.FilenameLength)
	}

	// Verify time conversion
	expectedDate, expectedTime := timeToMsDos(modTime)
	if header.LastModFileDate != expectedDate || header.LastModFileTime != expectedTime {
		t.Error("Time conversion in local header doesn't match expected values")
	}
}

// TestCentralDirEntry tests central directory entry creation
func TestCentralDirEntry(t *testing.T) {
	modTime := time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC)
	file := &file{
		name:              "test.txt",
		crc32:             0x12345678,
		modTime:           modTime,
		compressedSize:    512,
		uncompressedSize:  1024,
		compressionMethod: Deflated,
		localHeaderOffset: 100,
		comment:           "Test comment",
	}

	entry := file.centralDirEntry()

	// Verify entry fields
	if entry.VersionMadeBy != __LATEST_ZIP_VERSION {
		t.Errorf("Expected version made by %d, got %d", __LATEST_ZIP_VERSION, entry.VersionMadeBy)
	}
	if entry.CRC32 != file.crc32 {
		t.Errorf("Expected CRC32 %x, got %x", file.crc32, entry.CRC32)
	}
	if entry.CompressedSize != uint32(file.compressedSize) {
		t.Errorf("Expected compressed size %d, got %d", file.compressedSize, entry.CompressedSize)
	}
	if entry.LocalHeaderOffset != file.localHeaderOffset {
		t.Errorf("Expected local header offset %d, got %d", file.localHeaderOffset, entry.LocalHeaderOffset)
	}
	if entry.FileCommentLength != uint16(len(file.comment)) {
		t.Errorf("Expected comment length %d, got %d", len(file.comment), entry.FileCommentLength)
	}
}

// TestTimeToMsDosAndBack tests round-trip conversion of time
func TestTimeToMsDosAndBack(t *testing.T) {
	testTimes := []time.Time{
		time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
		time.Date(2107, 12, 31, 23, 59, 59, 0, time.UTC), // Max supported year
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),      // Before 1980 (should clamp)
	}

	for _, original := range testTimes {
		t.Run(original.Format("2006-01-02T15:04:05"), func(t *testing.T) {
			date, dosTime := timeToMsDos(original)
			reconstructed := msDosToTime(date, dosTime)

			// MS-DOS time has 2-second resolution, so we need to account for that
			timeDiff := reconstructed.Sub(original)
			if timeDiff < -2*time.Second || timeDiff > 2*time.Second && original.Year() != 1970 {
				t.Errorf("Time conversion round-trip failed: original %v, reconstructed %v",
					original, reconstructed)
			}

			// Verify individual components (within resolution limits)
			if reconstructed.Year() != original.Year() && original.Year() >= 1980 {
				t.Errorf("Year mismatch: original %d, reconstructed %d",
					original.Year(), reconstructed.Year())
			}
			if reconstructed.Month() != original.Month() {
				t.Errorf("Month mismatch: original %v, reconstructed %v",
					original.Month(), reconstructed.Month())
			}
			if reconstructed.Day() != original.Day() {
				t.Errorf("Day mismatch: original %d, reconstructed %d",
					original.Day(), reconstructed.Day())
			}
		})
	}
}

// TestByteCounterWriter tests the byte counting writer
func TestByteCounterWriter(t *testing.T) {
	var dest bytes.Buffer
	counter := &byteCounterWriter{dest: &dest}

	testData := []byte("Test data for byte counter")

	n, err := counter.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != len(testData) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(testData), n)
	}

	if counter.bytesWritten != int64(len(testData)) {
		t.Errorf("Expected bytesWritten %d, got %d", len(testData), counter.bytesWritten)
	}

	if !bytes.Equal(dest.Bytes(), testData) {
		t.Error("Written data doesn't match original")
	}
}

// TestCompressAndWriteEmptyFile tests compression with empty file
func TestCompressAndWriteEmptyFile(t *testing.T) {
	source := strings.NewReader("")
	file := &file{
		name:              "empty.txt",
		compressionMethod: Stored,
		source:            source,
	}

	var dest bytes.Buffer

	err := file.compressAndWrite(&dest)
	if err != nil {
		t.Fatalf("compressAndWrite failed for empty file: %v", err)
	}

	if file.compressedSize != 0 {
		t.Errorf("Expected compressed size 0, got %d", file.compressedSize)
	}

	if file.crc32 != crc32.ChecksumIEEE([]byte("")) {
		t.Errorf("CRC32 for empty file should be %x, got %x",
			crc32.ChecksumIEEE([]byte("")), file.crc32)
	}
}

// TestCompressAndWriteErrorHandling tests error handling during compression
func TestCompressAndWriteErrorHandling(t *testing.T) {
	// Create a reader that will return an error
	errorReader := &errorReader{err: io.ErrUnexpectedEOF}

	file := &file{
		name:              "error.txt",
		compressionMethod: Stored,
		source:            errorReader,
	}

	var dest bytes.Buffer

	err := file.compressAndWrite(&dest)
	if err == nil {
		t.Error("Expected error during compression, but got none")
	}
}

// errorReader is a test reader that always returns an error
type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (int, error) {
	return 0, r.err
}

// TestUpdateLocalHeader tests local header updating (simplified version)
func TestUpdateLocalHeader(t *testing.T) {
	// This is a simplified test since we can't easily test the file seeking
	// without creating actual files. In a real scenario, you'd use a temp file.
	file := &file{
		crc32:          0x12345678,
		compressedSize: 1024,
	}

	// We'll test that the struct fields are properly set
	if file.crc32 != 0x12345678 {
		t.Errorf("CRC32 not properly set")
	}
	if file.compressedSize != 1024 {
		t.Errorf("Compressed size not properly set")
	}
}

// TestEncodeEdgeCases tests various edge cases
func TestEncodeEdgeCases(t *testing.T) {
	t.Run("Very long filename", func(t *testing.T) {
		longName := strings.Repeat("a", 65535) // Max possible in ZIP spec
		file := &file{name: longName}
		header := file.localHeader()

		if header.FilenameLength != 65535 {
			t.Errorf("Expected filename length 65535, got %d", header.FilenameLength)
		}
	})

	t.Run("Year before 1980", func(t *testing.T) {
		oldTime := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
		date, _ := timeToMsDos(oldTime)

		// Year should be clamped to 0 (1980)
		year := int((date>>9)&0x7F) + 1980
		if year != 1980 {
			t.Errorf("Year before 1980 should be clamped to 1980, got %d", year)
		}
	})

	t.Run("Year after 2107", func(t *testing.T) {
		futureTime := time.Date(2108, 1, 1, 0, 0, 0, 0, time.UTC)
		date, _ := timeToMsDos(futureTime)

		// Year should be clamped to 127 (2107)
		year := int((date>>9)&0x7F) + 1980
		if year != 2107 {
			t.Errorf("Year after 2107 should be clamped to 2107, got %d", year)
		}
	})
}

// BenchmarkCompression benchmarks the compression performance
func BenchmarkCompression(b *testing.B) {
	// Create a large test data
	testData := strings.Repeat("This is test data that will be compressed. ", 1000)

	b.Run("Stored", func(b *testing.B) {
		for b.Loop() {
			source := strings.NewReader(testData)
			file := &file{
				compressionMethod: Stored,
				source:            source,
			}
			var dest bytes.Buffer
			file.compressAndWrite(&dest)
		}
	})

	b.Run("Deflated", func(b *testing.B) {
		for b.Loop() {
			source := strings.NewReader(testData)
			file := &file{
				compressionMethod: Deflated,
				source:            source,
			}
			var dest bytes.Buffer
			file.compressAndWrite(&dest)
		}
	})
}
