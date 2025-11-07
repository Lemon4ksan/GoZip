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

// TestFileCreation tests various file creation scenarios
func TestFileCreation(t *testing.T) {
	t.Run("FromOSFile", func(t *testing.T) {
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
	})

	t.Run("DirectoryFile", func(t *testing.T) {
		dirFile, err := newDirectoryFile("", "testdir")
		if err != nil {
			t.Fatalf("Failed to create directory file: %v", err)
		}

		if !dirFile.isDir {
			t.Error("Directory file should have isDir = true")
		}
		if dirFile.name != "testdir" {
			t.Errorf("Expected name 'testdir', got '%s'", dirFile.name)
		}
		if dirFile.uncompressedSize != 0 {
			t.Errorf("Directory should have size 0, got %d", dirFile.uncompressedSize)
		}
	})
}

// TestCompressionMethods tests different compression methods and scenarios
func TestCompressionMethods(t *testing.T) {
	testCases := []struct {
		name        string
		method      CompressionMethod
		content     string
		description string
	}{
		{
			name:        "Stored",
			method:      Stored,
			content:     "Test content for compression",
			description: "no compression",
		},
		{
			name:        "Deflated",
			method:      Deflated,
			content:     "This is a test content that should compress well because it has repeated patterns and is somewhat longer than just a few bytes to demonstrate compression working.",
			description: "with compression",
		},
		{
			name:        "Empty",
			method:      Stored,
			content:     "",
			description: "empty file",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.NewReader(tc.content)
			file := &file{
				name:              "test.txt",
				compressionMethod: tc.method,
				source:            source,
			}

			var dest bytes.Buffer
			err := file.compressAndWrite(&dest)
			if err != nil {
				t.Fatalf("compressAndWrite failed for %s: %v", tc.description, err)
			}

			// Verify CRC32 checksum
			expectedCRC := crc32.ChecksumIEEE([]byte(tc.content))
			if file.crc32 != expectedCRC {
				t.Errorf("Expected CRC32 %x, got %x", expectedCRC, file.crc32)
			}

			// Size verification based on method
			switch tc.method {
			case Stored:
				if file.compressedSize != int64(len(tc.content)) {
					t.Errorf("Stored: expected compressed size %d, got %d", len(tc.content), file.compressedSize)
				}
				if tc.content != "" && dest.String() != tc.content {
					t.Error("Stored: compressed content doesn't match original")
				}
			case Deflated:
				if file.compressedSize > int64(len(tc.content)) {
					t.Errorf("Deflated: compressed size %d should be <= original size %d",
						file.compressedSize, len(tc.content))
				}
			}
		})
	}

	t.Run("ErrorHandling", func(t *testing.T) {
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
	})
}

// TestFileHeaders tests local and central directory header creation
func TestFileHeaders(t *testing.T) {
	modTime := time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC)
	
	testCases := []struct {
		name     string
		file     *file
		testType string // "local" or "central"
	}{
		{
			name: "RegularFile",
			file: &file{
				name:              "test.txt",
				uncompressedSize:  1024,
				modTime:           modTime,
				compressionMethod: Deflated,
			},
			testType: "local",
		},
		{
			name: "CentralDirectory",
			file: &file{
				name:              "test.txt",
				crc32:             0x12345678,
				modTime:           modTime,
				compressedSize:    512,
				uncompressedSize:  1024,
				compressionMethod: Deflated,
				localHeaderOffset: 100,
				comment:           "Test comment",
			},
			testType: "central",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.testType {
			case "local":
				header := tc.file.localHeader()
				if header.VersionNeededToExtract != 20 {
					t.Errorf("Expected version 20 for Deflated, got %d", header.VersionNeededToExtract)
				}
				if header.CompressionMethod != uint16(Deflated) {
					t.Errorf("Expected compression method %d, got %d", Deflated, header.CompressionMethod)
				}
				if header.UncompressedSize != 1024 {
					t.Errorf("Expected uncompressed size 1024, got %d", header.UncompressedSize)
				}
				
				expectedDate, expectedTime := timeToMsDos(modTime)
				if header.LastModFileDate != expectedDate || header.LastModFileTime != expectedTime {
					t.Error("Time conversion in local header doesn't match expected values")
				}

			case "central":
				entry := tc.file.centralDirEntry()
				if entry.VersionMadeBy != __LATEST_ZIP_VERSION {
					t.Errorf("Expected version made by %d, got %d", __LATEST_ZIP_VERSION, entry.VersionMadeBy)
				}
				if entry.CRC32 != tc.file.crc32 {
					t.Errorf("Expected CRC32 %x, got %x", tc.file.crc32, entry.CRC32)
				}
				if entry.CompressedSize != uint32(tc.file.compressedSize) {
					t.Errorf("Expected compressed size %d, got %d", tc.file.compressedSize, entry.CompressedSize)
				}
				if entry.LocalHeaderOffset != tc.file.localHeaderOffset {
					t.Errorf("Expected local header offset %d, got %d", tc.file.localHeaderOffset, entry.LocalHeaderOffset)
				}
			}
		})
	}
}

// TestDirectoryOperations tests directory-related functionality
func TestDirectoryOperations(t *testing.T) {
	t.Run("PathHandling", func(t *testing.T) {
		pathTests := []struct {
			name     string
			path     string
			filename string
			isDir    bool
			expected string
		}{
			{"DirectoryInRoot", "", "docs", true, "docs/"},
			{"DirectoryWithPath", "projects", "src", true, "projects/src/"},
			{"NestedDirectory", "a/b/c", "d", true, "a/b/c/d/"},
			{"RegularFile", "folder", "file.txt", false, "folder/file.txt"},
		}

		for _, tt := range pathTests {
			t.Run(tt.name, func(t *testing.T) {
				f := &file{name: tt.filename, path: tt.path, isDir: tt.isDir, modTime: time.Now()}
				header := f.localHeader()
				expectedLength := len(tt.expected)
				if int(header.FilenameLength) != expectedLength {
					t.Errorf("Expected filename length %d for '%s', got %d",
						expectedLength, tt.expected, header.FilenameLength)
				}
			})
		}
	})

	t.Run("Compression", func(t *testing.T) {
		dirFile := &file{
			name:              "empty",
			isDir:             true,
			compressionMethod: Stored,
			source:            bytes.NewReader(nil),
		}

		var dest bytes.Buffer
		err := dirFile.compressAndWrite(&dest)
		if err != nil {
			t.Errorf("Compressing directory should not fail: %v", err)
		}

		// Directory should have zero sizes and use Stored compression
		if dirFile.compressionMethod != Stored {
			t.Errorf("Directories should use Stored compression, got %v", dirFile.compressionMethod)
		}
		if dirFile.compressedSize != 0 || dirFile.uncompressedSize != 0 || dirFile.crc32 != 0 {
			t.Errorf("Directory should have zero sizes and CRC, got compressed=%d, uncompressed=%d, crc=%x",
				dirFile.compressedSize, dirFile.uncompressedSize, dirFile.crc32)
		}
	})
}

// TestVersionAndFlags tests version detection and bit flags
func TestVersionAndFlags(t *testing.T) {
	t.Run("VersionDetection", func(t *testing.T) {
		versionTests := []struct {
			name     string
			file     *file
			expected uint16
		}{
			{
				name: "DirectoryWithoutPath",
				file: &file{name: "dir", isDir: true, path: ""},
				expected: 20,
			},
			{
				name: "RegularFileStored",
				file: &file{name: "file.txt", isDir: false, compressionMethod: Stored},
				expected: 10,
			},
			{
				name: "RegularFileDeflated",
				file: &file{name: "file.txt", isDir: false, compressionMethod: Deflated},
				expected: 20,
			},
		}

		for _, tt := range versionTests {
			t.Run(tt.name, func(t *testing.T) {
				version := tt.file.getVersionNeededToExtract()
				if version != tt.expected {
					t.Errorf("Expected version %d, got %d for %s", tt.expected, version, tt.name)
				}
			})
		}
	})

	t.Run("BitFlags", func(t *testing.T) {
		flagTests := []struct {
			name     string
			file     *file
			expected uint16
		}{
			{
				name:     "DeflateNormal",
				file:     &file{compressionMethod: Deflated, compressionLevel: DeflateNormal},
				expected: 0x0000,
			},
			{
				name:     "DeflateMaximum", 
				file:     &file{compressionMethod: Deflated, compressionLevel: DeflateMaximum},
				expected: 0x0002,
			},
			{
				name:     "EncryptedDirectory",
				file:     &file{name: "secret", isDir: true, isEncrypted: true},
				expected: 0x0001,
			},
		}

		for _, tt := range flagTests {
			t.Run(tt.name, func(t *testing.T) {
				flags := tt.file.getFileBitFlag()
				if flags != tt.expected {
					t.Errorf("%s: expected 0x%04X, got 0x%04X", tt.name, tt.expected, flags)
				}
			})
		}
	})
}

// TestEdgeCases tests various edge cases and boundary conditions
func TestEdgeCases(t *testing.T) {
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

	t.Run("FilenameLength", func(t *testing.T) {
		longName := strings.Repeat("a", 65535) // Max possible in ZIP spec
		file := &file{name: longName}
		header := file.localHeader()

		if header.FilenameLength != 65535 {
			t.Errorf("Expected filename length 65535, got %d", header.FilenameLength)
		}
	})

	t.Run("MixedFilesAndDirectories", func(t *testing.T) {
		files := []*file{
			{name: "file1.txt", isDir: false, uncompressedSize: 100},
			{name: "docs", isDir: true},
			{name: "file2.txt", isDir: false, uncompressedSize: 200},
		}

		zip := &Zip{files: files}
		encoded := encodeEndOfCentralDirRecord(zip, 1024, 2048)

		var record endOfCentralDirectory
		buf := bytes.NewReader(encoded[4:])
		binary.Read(buf, binary.LittleEndian, &record)

		if record.TotalNumberOfEntries != 3 {
			t.Errorf("Should count both files and directories, expected 3, got %d",
				record.TotalNumberOfEntries)
		}
	})
}

// TestByteCounter tests the byte counting writer
func TestByteCounter(t *testing.T) {
	var dest bytes.Buffer
	counter := &byteCounterWriter{dest: &dest}

	testData := []byte("Test data for byte counter")
	n, err := counter.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != len(testData) || counter.bytesWritten != int64(len(testData)) {
		t.Errorf("Expected to write %d bytes, wrote %d, counter shows %d", 
			len(testData), n, counter.bytesWritten)
	}

	if !bytes.Equal(dest.Bytes(), testData) {
		t.Error("Written data doesn't match original")
	}
}

// Benchmark tests
func BenchmarkCompression(b *testing.B) {
	testData := strings.Repeat("This is test data that will be compressed. ", 1000)

	benchmarks := []struct {
		name     string
		method   CompressionMethod
	}{
		{"Stored", Stored},
		{"Deflated", Deflated},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				source := strings.NewReader(testData)
				file := &file{compressionMethod: bm.method, source: source}
				var dest bytes.Buffer
				file.compressAndWrite(&dest)
			}
		})
	}
}

func BenchmarkDirectoryOperations(b *testing.B) {
	dirFile := &file{
		name:    "benchdir", 
		isDir:   true,
		path:    "benchmark/path",
		modTime: time.Now(),
	}

	operations := []struct {
		name string
		fn   func()
	}{
		{"LocalHeader", func() { dirFile.localHeader() }},
		{"CentralDirEntry", func() { dirFile.centralDirEntry() }},
		{"VersionDetection", func() { dirFile.getVersionNeededToExtract() }},
		{"BitFlags", func() { dirFile.getFileBitFlag() }},
	}

	for _, op := range operations {
		b.Run(op.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				op.fn()
			}
		})
	}
}

// errorReader is a test reader that always returns an error
type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (int, error) {
	return 0, r.err
}
