package gozip

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// TestFileHeaderEncoding tests encoding of local file headers for both files and directories
func TestFileHeaderEncoding(t *testing.T) {
	tests := []struct {
		name        string
		header      localFileHeader
		file        *file
		description string
	}{
		{
			name: "Regular file with deflate compression",
			header: localFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Deflated),
				LastModFileTime:        0x4D34,
				LastModFileDate:        0x5521,
				CRC32:                  0x12345678,
				CompressedSize:         1024,
				UncompressedSize:       2048,
				FilenameLength:         8,
				ExtraFieldLength:       0,
			},
			file:        &file{name: "test.txt"},
			description: "Basic file with deflate compression",
		},
		{
			name: "Regular file with stored compression",
			header: localFileHeader{
				VersionNeededToExtract: 10,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Stored),
				LastModFileTime:        0x1234,
				LastModFileDate:        0x5678,
				CRC32:                  0xABCDEF01,
				CompressedSize:         100,
				UncompressedSize:       100,
				FilenameLength:         12,
				ExtraFieldLength:       0,
			},
			file:        &file{name: "document.doc"},
			description: "File with stored (no) compression",
		},
		{
			name: "Directory with path",
			header: localFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Stored),
				LastModFileTime:        0x4D34,
				LastModFileDate:        0x5521,
				CRC32:                  0,
				CompressedSize:         0,
				UncompressedSize:       0,
				FilenameLength:         uint16(len("projects/docs/")),
				ExtraFieldLength:       0,
			},
			file:        &file{name: "docs", isDir: true, path: "projects"},
			description: "Directory with project path",
		},
		{
			name: "Root directory",
			header: localFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Stored),
				LastModFileTime:        0x1234,
				LastModFileDate:        0x5678,
				CRC32:                  0,
				CompressedSize:         0,
				UncompressedSize:       0,
				FilenameLength:         1,
				ExtraFieldLength:       0,
			},
			file:        &file{name: "", isDir: true, path: ""},
			description: "Root directory entry",
		},
		{
			name: "Empty filename file",
			header: localFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Deflated),
				LastModFileTime:        0x1111,
				LastModFileDate:        0x2222,
				CRC32:                  0x33333333,
				CompressedSize:         0,
				UncompressedSize:       0,
				FilenameLength:         0,
				ExtraFieldLength:       0,
			},
			file:        &file{name: ""},
			description: "File with empty filename",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.header.encode(tt.file)

			// Verify signature
			var signature uint32
			buf := bytes.NewReader(encoded)
			binary.Read(buf, binary.LittleEndian, &signature)
			if signature != __LOCAL_FILE_HEADER_SIGNATURE {
				t.Errorf("Expected signature %x, got %x", __LOCAL_FILE_HEADER_SIGNATURE, signature)
			}

			// Verify header structure
			var decodedHeader localFileHeader
			err := binary.Read(buf, binary.LittleEndian, &decodedHeader)
			if err != nil {
				t.Fatalf("Failed to decode header: %v", err)
			}

			// Compare all fields
			if decodedHeader.VersionNeededToExtract != tt.header.VersionNeededToExtract {
				t.Errorf("VersionNeededToExtract mismatch: expected %d, got %d",
					tt.header.VersionNeededToExtract, decodedHeader.VersionNeededToExtract)
			}
			if decodedHeader.CompressionMethod != tt.header.CompressionMethod {
				t.Errorf("CompressionMethod mismatch: expected %d, got %d",
					tt.header.CompressionMethod, decodedHeader.CompressionMethod)
			}
			if decodedHeader.CRC32 != tt.header.CRC32 {
				t.Errorf("CRC32 mismatch: expected %x, got %x", tt.header.CRC32, decodedHeader.CRC32)
			}

			// Verify filename
			filenameBytes := make([]byte, tt.header.FilenameLength)
			buf.Read(filenameBytes)
			expectedFilename := tt.header.getExpectedFilename(tt.file)
			if string(filenameBytes) != expectedFilename {
				t.Errorf("Filename mismatch: expected '%s', got '%s'", expectedFilename, string(filenameBytes))
			}

			// Verify total length
			expectedLength := 4 + binary.Size(tt.header) + len(expectedFilename)
			if len(encoded) != expectedLength {
				t.Errorf("Encoded length mismatch: expected %d, got %d", expectedLength, len(encoded))
			}

			// Directory-specific validations
			if tt.file.isDir {
				if decodedHeader.CompressedSize != 0 {
					t.Errorf("Directory compressed size should be 0, got %d", decodedHeader.CompressedSize)
				}
				if decodedHeader.UncompressedSize != 0 {
					t.Errorf("Directory uncompressed size should be 0, got %d", decodedHeader.UncompressedSize)
				}
				if !strings.HasSuffix(string(filenameBytes), "/") {
					t.Errorf("Directory filename should end with slash: '%s'", string(filenameBytes))
				}
			}
		})
	}
}

// Helper method to calculate expected filename
func (h localFileHeader) getExpectedFilename(f *file) string {
	filename := f.name
	if f.path != "" {
		filename = f.path + "/" + filename
	}
	if f.isDir {
		filename += "/"
	}
	return filename
}

// TestCentralDirectoryEncoding tests encoding of central directory entries for files and directories
func TestCentralDirectoryEncoding(t *testing.T) {
	tests := []struct {
		name        string
		dir         centralDirectory
		file        *file
		description string
	}{
		{
			name: "Complete file directory entry",
			dir: centralDirectory{
				VersionMadeBy:          63,
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Deflated),
				LastModFileTime:        0x4D34,
				LastModFileDate:        0x5521,
				CRC32:                  0x12345678,
				CompressedSize:         1024,
				UncompressedSize:       2048,
				FilenameLength:         8,
				ExtraFieldLength:       0,
				FileCommentLength:      0,
				DiskNumberStart:        0,
				InternalFileAttributes: 0,
				ExternalFileAttributes: 0,
				LocalHeaderOffset:      256,
			},
			file:        &file{name: "test.txt"},
			description: "Standard file entry",
		},
		{
			name: "File with attributes",
			dir: centralDirectory{
				VersionMadeBy:          63,
				VersionNeededToExtract: 10,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Stored),
				LastModFileTime:        0x1111,
				LastModFileDate:        0x2222,
				CRC32:                  0x33333333,
				CompressedSize:         500,
				UncompressedSize:       500,
				FilenameLength:         15,
				ExtraFieldLength:       0,
				FileCommentLength:      0,
				DiskNumberStart:        1,
				InternalFileAttributes: 1,
				ExternalFileAttributes: 0x81A40000,
				LocalHeaderOffset:      1024,
			},
			file:        &file{name: "executable.bin"},
			description: "File with execution attributes",
		},
		{
			name: "Directory with comment",
			dir: centralDirectory{
				VersionMadeBy:          63,
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      uint16(Stored),
				LastModFileTime:        0x4D34,
				LastModFileDate:        0x5521,
				CRC32:                  0,
				CompressedSize:         0,
				UncompressedSize:       0,
				FilenameLength:         uint16(len("app/src/")),
				ExtraFieldLength:       0,
				FileCommentLength:      uint16(len("Source code directory")),
				DiskNumberStart:        0,
				InternalFileAttributes: 0,
				ExternalFileAttributes: 0,
				LocalHeaderOffset:      2048,
			},
			file:        &file{name: "src", isDir: true, path: "app", config: FileConfig{Comment: "Source code directory"}},
			description: "Directory with descriptive comment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.dir.encode(tt.file)

			// Verify signature
			var signature uint32
			buf := bytes.NewReader(encoded)
			binary.Read(buf, binary.LittleEndian, &signature)
			if signature != __CENTRAL_DIRECTORY_SIGNATURE {
				t.Errorf("Expected signature %x, got %x", __CENTRAL_DIRECTORY_SIGNATURE, signature)
			}

			// Verify directory structure
			var decodedDir centralDirectory
			err := binary.Read(buf, binary.LittleEndian, &decodedDir)
			if err != nil {
				t.Fatalf("Failed to decode directory: %v", err)
			}

			// Compare critical fields
			if decodedDir.VersionMadeBy != tt.dir.VersionMadeBy {
				t.Errorf("VersionMadeBy mismatch: expected %d, got %d", tt.dir.VersionMadeBy, decodedDir.VersionMadeBy)
			}
			if decodedDir.CRC32 != tt.dir.CRC32 {
				t.Errorf("CRC32 mismatch: expected %x, got %x", tt.dir.CRC32, decodedDir.CRC32)
			}
			if decodedDir.LocalHeaderOffset != tt.dir.LocalHeaderOffset {
				t.Errorf("LocalHeaderOffset mismatch: expected %d, got %d", tt.dir.LocalHeaderOffset, decodedDir.LocalHeaderOffset)
			}

			// Verify filename
			filenameBytes := make([]byte, tt.dir.FilenameLength)
			buf.Read(filenameBytes)
			expectedFilename := tt.dir.getExpectedFilename(tt.file)
			if len(filenameBytes) > 0 && filenameBytes[len(filenameBytes)-1] == '\x00' {
				filenameBytes = filenameBytes[:len(filenameBytes)-1]
			}
			if string(filenameBytes) != expectedFilename {
				t.Errorf("Filename mismatch: expected '%s', got '%s'", expectedFilename, string(filenameBytes))
			}

			// Verify comment if present
			if tt.dir.FileCommentLength > 0 {
				commentBytes := make([]byte, tt.dir.FileCommentLength)
				buf.Read(commentBytes)
				if string(commentBytes) != tt.file.config.Comment {
					t.Errorf("Comment mismatch: expected '%s', got '%s'", tt.file.config.Comment, string(commentBytes))
				}
			}

			// Directory-specific validations
			if tt.file.isDir {
				if decodedDir.CompressedSize != 0 {
					t.Errorf("Directory compressed size should be 0, got %d", decodedDir.CompressedSize)
				}
				if decodedDir.UncompressedSize != 0 {
					t.Errorf("Directory uncompressed size should be 0, got %d", decodedDir.UncompressedSize)
				}
				if !strings.HasSuffix(string(filenameBytes), "/") {
					t.Errorf("Directory filename should end with slash: '%s'", string(filenameBytes))
				}
			}
		})
	}
}

// Helper method to calculate expected filename for central directory
func (d centralDirectory) getExpectedFilename(f *file) string {
	filename := f.name
	if f.path != "" {
		filename = f.path + "/" + filename
	}
	if f.isDir {
		filename += "/"
	}
	return filename
}

// TestArchiveStructureComprehensive tests end-of-central-directory and signature constants
func TestArchiveStructureComprehensive(t *testing.T) {
	t.Run("EndOfCentralDirectory variations", func(t *testing.T) {
		testCases := []struct {
			name               string
			files              []*file
			comment            string
			centralDirSize     int64
			centralDirOffset   int64
			expectedTotalFiles uint16
			description        string
		}{
			{
				name:               "Single file archive",
				files:              []*file{{name: "file1.txt"}},
				comment:            "",
				centralDirSize:     1024,
				centralDirOffset:   2048,
				expectedTotalFiles: 1,
				description:        "Basic single file archive",
			},
			{
				name: "Mixed content archive",
				files: []*file{
					{name: "readme.txt", isDir: false},
					{name: "src", isDir: true, path: "app"},
					{name: "main.go", isDir: false, path: "app/src"},
					{name: "docs", isDir: true},
				},
				comment:            "Project archive",
				centralDirSize:     3072,
				centralDirOffset:   8192,
				expectedTotalFiles: 4,
				description:        "Archive with files and directories",
			},
			{
				name:               "Empty archive with comment",
				files:              []*file{},
				comment:            "Empty archive placeholder",
				centralDirSize:     0,
				centralDirOffset:   0,
				expectedTotalFiles: 0,
				description:        "Archive with no files but with comment",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				zip := &Zip{
					files:   tc.files,
					comment: tc.comment,
				}

				encoded := encodeEndOfCentralDirRecord(zip, tc.centralDirSize, tc.centralDirOffset)

				// Verify signature
				var signature uint32
				buf := bytes.NewReader(encoded)
				binary.Read(buf, binary.LittleEndian, &signature)
				if signature != __END_OF_CENTRAL_DIRECTORY_SIGNATURE {
					t.Errorf("Expected signature %x, got %x", __END_OF_CENTRAL_DIRECTORY_SIGNATURE, signature)
				}

				// Verify record structure
				var record endOfCentralDirectory
				err := binary.Read(buf, binary.LittleEndian, &record)
				if err != nil {
					t.Fatalf("Failed to decode end record: %v", err)
				}

				// Verify record fields
				if record.TotalNumberOfEntries != tc.expectedTotalFiles {
					t.Errorf("TotalNumberOfEntries mismatch: expected %d, got %d",
						tc.expectedTotalFiles, record.TotalNumberOfEntries)
				}
				if int64(record.CentralDirSize) != tc.centralDirSize {
					t.Errorf("CentralDirSize mismatch: expected %d, got %d", tc.centralDirSize, record.CentralDirSize)
				}
				if int64(record.CentralDirOffset) != tc.centralDirOffset {
					t.Errorf("CentralDirOffset mismatch: expected %d, got %d", tc.centralDirOffset, record.CentralDirOffset)
				}

				// Verify comment
				if len(tc.comment) > 0 {
					commentBytes := make([]byte, record.CommentLength)
					buf.Read(commentBytes)
					if string(commentBytes) != tc.comment {
						t.Errorf("Comment mismatch: expected '%s', got '%s'", tc.comment, string(commentBytes))
					}
				}
			})
		}
	})

	t.Run("Signature constants validation", func(t *testing.T) {
		signatureTests := []struct {
			name      string
			signature uint32
			expected  string
		}{
			{"Central Directory", __CENTRAL_DIRECTORY_SIGNATURE, "PK\x01\x02"},
			{"Local File Header", __LOCAL_FILE_HEADER_SIGNATURE, "PK\x03\x04"},
			{"Digital Signature", __DIGITAL_HEADER_SIGNATURE, "PK\x05\x05"},
			{"End of Central Directory", __END_OF_CENTRAL_DIRECTORY_SIGNATURE, "PK\x05\x06"},
			{"Zip64 End of Central Directory", __ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE, "PK\x06\x06"},
			{"Zip64 Directory Locator", __ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE, "PK\x06\x07"},
			{"Archive Extra Data", __ARCHIVE_EXTRA_DATA_SIGNATURE, "PK\x06\x08"},
		}

		for _, tt := range signatureTests {
			t.Run(tt.name, func(t *testing.T) {
				buf := make([]byte, 4)
				binary.LittleEndian.PutUint32(buf, tt.signature)

				if buf[0] != 'P' || buf[1] != 'K' {
					t.Errorf("Signature %x doesn't start with PK: got %s", tt.signature, string(buf[:2]))
				}

				actualString := string(buf)
				if actualString != tt.expected {
					t.Errorf("Signature string mismatch: expected %q, got %q", tt.expected, actualString)
				}
			})
		}
	})
}

// TestPathAndEdgeCases tests various path combinations and edge cases
func TestPathAndEdgeCases(t *testing.T) {
	t.Run("Path combinations for files and directories", func(t *testing.T) {
		testCases := []struct {
			name     string
			filePath string
			fileName string
			isDir    bool
			expected string
		}{
			{"File in root", "", "file.txt", false, "file.txt"},
			{"File with path", "folder", "file.txt", false, "folder/file.txt"},
			{"Directory in root", "", "docs", true, "docs/"},
			{"Directory with path", "projects", "src", true, "projects/src/"},
			{"Nested directory", "a/b/c", "d", true, "a/b/c/d/"},
			{"Root directory", "", "", true, "/"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				f := &file{
					name:    tc.fileName,
					path:    tc.filePath,
					isDir:   tc.isDir,
					modTime: time.Now(),
				}

				header := localFileHeader{
					FilenameLength: uint16(len(tc.expected)),
				}

				encoded := header.encode(f)

				// Extract filename from encoded data
				buf := bytes.NewReader(encoded)
				buf.Seek(4+int64(binary.Size(header)), 0)
				filenameBytes := make([]byte, header.FilenameLength)
				buf.Read(filenameBytes)

				if string(filenameBytes) != tc.expected {
					t.Errorf("Path combination mismatch: expected '%s', got '%s'",
						tc.expected, string(filenameBytes))
				}
			})
		}
	})

	t.Run("Encoding consistency between local and central", func(t *testing.T) {
		testFiles := []*file{
			{name: "file.txt", isDir: false, path: "docs"},
			{name: "src", isDir: true, path: "app"},
			{name: "", isDir: true, path: ""}, // root directory
		}

		for _, f := range testFiles {
			t.Run(f.name+" consistency", func(t *testing.T) {
				h := newZipHeaders(f)
				localHeader := h.LocalHeader()
				centralDir := h.CentralDirEntry()

				localEncoded := localHeader.encode(f)
				centralEncoded := centralDir.encode(f)

				// Extract filenames from both encodings
				localFilename := extractFilename(localEncoded, localHeader)
				centralFilename := extractFilename(centralEncoded, centralDir)

				if localFilename != centralFilename {
					t.Errorf("Filename consistency failed: local '%s', central '%s'", localFilename, centralFilename)
				}

				if f.isDir && !strings.HasSuffix(localFilename, "/") {
					t.Errorf("Directory should end with slash: '%s'", localFilename)
				}
			})
		}
	})

	t.Run("Edge cases and boundary conditions", func(t *testing.T) {
		t.Run("Very long filenames", func(t *testing.T) {
			longName := strings.Repeat("a", 255)
			dir := centralDirectory{FilenameLength: uint16(len(longName))}
			encoded := dir.encode(&file{name: longName})

			if len(encoded) < 4+42+len(longName) {
				t.Errorf("Encoded data too short for long filename")
			}
		})

		t.Run("Maximum values in structures", func(t *testing.T) {
			zip := &Zip{files: make([]*file, 1000)} // Reasonable large number
			encoded := encodeEndOfCentralDirRecord(zip, 0xFFFFFFFF, 0xFFFFFFFF)

			var record endOfCentralDirectory
			buf := bytes.NewReader(encoded[4:])
			binary.Read(buf, binary.LittleEndian, &record)

			if record.CentralDirSize != 0xFFFFFFFF {
				t.Errorf("Large CentralDirSize not preserved")
			}
		})
	})
}

// Helper function to extract filename from encoded data
func extractFilename(encoded []byte, header interface{}) string {
	buf := bytes.NewReader(encoded)

	// Skip signature and header based on type
	switch h := header.(type) {
	case localFileHeader:
		buf.Seek(4+int64(binary.Size(h)), 0)
		filenameBytes := make([]byte, h.FilenameLength)
		buf.Read(filenameBytes)
		return string(filenameBytes)
	case centralDirectory:
		buf.Seek(4+int64(binary.Size(h)), 0)
		filenameBytes := make([]byte, h.FilenameLength)
		buf.Read(filenameBytes)
		return string(filenameBytes)
	default:
		return ""
	}
}

// Benchmark tests for performance measurement
func BenchmarkStructureEncoding(b *testing.B) {
	// Common test data
	fileHeader := localFileHeader{
		VersionNeededToExtract: 20,
		CompressionMethod:      uint16(Deflated),
		CRC32:                  0x12345678,
		CompressedSize:         1024,
		UncompressedSize:       2048,
		FilenameLength:         8,
	}

	centralDir := centralDirectory{
		VersionMadeBy:          63,
		VersionNeededToExtract: 20,
		CompressionMethod:      uint16(Deflated),
		CRC32:                  0x12345678,
		CompressedSize:         1024,
		UncompressedSize:       2048,
		FilenameLength:         8,
		LocalHeaderOffset:      256,
	}

	regularFile := &file{name: "test.txt"}
	directoryFile := &file{name: "docs", isDir: true, path: "projects"}

	zip := &Zip{
		files:   []*file{regularFile},
		comment: "benchmark",
	}

	b.ResetTimer()

	// File encoding benchmarks
	b.Run("FileLocalHeaderEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			fileHeader.encode(regularFile)
		}
	})

	b.Run("FileCentralDirEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			centralDir.encode(regularFile)
		}
	})

	// Directory encoding benchmarks
	b.Run("DirectoryLocalHeaderEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			fileHeader.encode(directoryFile)
		}
	})

	b.Run("DirectoryCentralDirEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			centralDir.encode(directoryFile)
		}
	})

	// End of central directory benchmark
	b.Run("EndOfCentralDirEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			encodeEndOfCentralDirRecord(zip, 1024, 2048)
		}
	})
}
