package gozip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestLocalFileHeaderEncode tests encoding of local file header
func TestLocalFileHeaderEncode(t *testing.T) {
	tests := []struct {
		name     string
		header   localFileHeader
		filename string
	}{
		{
			name: "Basic header",
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
			filename: "test.txt",
		},
		{
			name: "Stored compression",
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
			filename: "document.doc",
		},
		{
			name: "Empty filename",
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
			filename: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.header.encode(&file{name: tt.filename})

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
				t.Errorf("versionNeededToExtract mismatch: expected %d, got %d",
					tt.header.VersionNeededToExtract, decodedHeader.VersionNeededToExtract)
			}
			if decodedHeader.CompressionMethod != tt.header.CompressionMethod {
				t.Errorf("compressionMethod mismatch: expected %d, got %d",
					tt.header.CompressionMethod, decodedHeader.CompressionMethod)
			}
			if decodedHeader.CRC32 != tt.header.CRC32 {
				t.Errorf("crc32 mismatch: expected %x, got %x", tt.header.CRC32, decodedHeader.CRC32)
			}
			if decodedHeader.CompressedSize != tt.header.CompressedSize {
				t.Errorf("compressedSize mismatch: expected %d, got %d",
					tt.header.CompressedSize, decodedHeader.CompressedSize)
			}

			// Verify filename
			filenameBytes := make([]byte, tt.header.FilenameLength)
			buf.Read(filenameBytes)
			if string(filenameBytes) != tt.filename {
				t.Errorf("Filename mismatch: expected %s, got %s", tt.filename, string(filenameBytes))
			}

			// Verify total length
			expectedLength := 4 + binary.Size(tt.header) + len(tt.filename) // signature + header + filename
			if len(encoded) != expectedLength {
				t.Errorf("Encoded length mismatch: expected %d, got %d", expectedLength, len(encoded))
			}
		})
	}
}

// TestCentralDirectoryEncode tests encoding of central directory structure
func TestCentralDirectoryEncode(t *testing.T) {
	tests := []struct {
		name     string
		dir      centralDirectory
		filename string
	}{
		{
			name: "Complete directory entry",
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
			filename: "test.txt",
		},
		{
			name: "With file attributes",
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
				ExternalFileAttributes: 0x81A40000, // Typical file attributes
				LocalHeaderOffset:      1024,
			},
			filename: "executable.bin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.dir.encode(&file{name: tt.filename})

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
				t.Errorf("versionMadeBy mismatch: expected %d, got %d",
					tt.dir.VersionMadeBy, decodedDir.VersionMadeBy)
			}
			if decodedDir.CRC32 != tt.dir.CRC32 {
				t.Errorf("crc32 mismatch: expected %x, got %x", tt.dir.CRC32, decodedDir.CRC32)
			}
			if decodedDir.LocalHeaderOffset != tt.dir.LocalHeaderOffset {
				t.Errorf("localHeaderOffset mismatch: expected %d, got %d",
					tt.dir.LocalHeaderOffset, decodedDir.LocalHeaderOffset)
			}
			if decodedDir.ExternalFileAttributes != tt.dir.ExternalFileAttributes {
				t.Errorf("externalFileAttributes mismatch: expected %x, got %x",
					tt.dir.ExternalFileAttributes, decodedDir.ExternalFileAttributes)
			}

			// Verify filename
			filenameBytes := make([]byte, tt.dir.FilenameLength)
			buf.Read(filenameBytes)
			if len(filenameBytes) > 0 && filenameBytes[len(filenameBytes)-1] == '\x00' {
				filenameBytes = filenameBytes[:len(filenameBytes)-1]
			}
			if string(filenameBytes) != tt.filename {
				t.Errorf("Filename mismatch: expected %s, got %s", tt.filename, string(filenameBytes))
			}
		})
	}
}

// TestEncodeEndOfCentralDirRecord tests end of central directory record encoding
func TestEncodeEndOfCentralDirRecord(t *testing.T) {
	tests := []struct {
		name               string
		files              []*file
		comment            string
		centralDirSize     uint32
		centralDirOffset   uint32
		expectedTotalFiles uint16
	}{
		{
			name:               "Single file no comment",
			files:              []*file{{name: "file1.txt"}},
			comment:            "",
			centralDirSize:     1024,
			centralDirOffset:   2048,
			expectedTotalFiles: 1,
		},
		{
			name: "Multiple files with comment",
			files: []*file{
				{name: "file1.txt"},
				{name: "file2.txt"},
				{name: "file3.txt"},
			},
			comment:            "Test archive comment",
			centralDirSize:     3072,
			centralDirOffset:   8192,
			expectedTotalFiles: 3,
		},
		{
			name:               "Empty archive",
			files:              []*file{},
			comment:            "Empty archive",
			centralDirSize:     0,
			centralDirOffset:   0,
			expectedTotalFiles: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zip := &Zip{
				files:   tt.files,
				comment: tt.comment,
			}

			encoded := encodeEndOfCentralDirRecord(zip, tt.centralDirSize, tt.centralDirOffset)

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
			if record.TotalNumberOfEntries != tt.expectedTotalFiles {
				t.Errorf("totalNumberOfEntries mismatch: expected %d, got %d",
					tt.expectedTotalFiles, record.TotalNumberOfEntries)
			}
			if record.TotalNumberOfEntriesOnThisDisk != tt.expectedTotalFiles {
				t.Errorf("totalNumberOfEntriesOnThisDisk mismatch: expected %d, got %d",
					tt.expectedTotalFiles, record.TotalNumberOfEntriesOnThisDisk)
			}
			if record.CentralDirSize != tt.centralDirSize {
				t.Errorf("centralDirSize mismatch: expected %d, got %d",
					tt.centralDirSize, record.CentralDirSize)
			}
			if record.CentralDirOffset != tt.centralDirOffset {
				t.Errorf("centralDirOffset mismatch: expected %d, got %d",
					tt.centralDirOffset, record.CentralDirOffset)
			}
			if record.CommentLength != uint16(len(tt.comment)) {
				t.Errorf("commentLength mismatch: expected %d, got %d",
					len(tt.comment), record.CommentLength)
			}

			// Verify comment
			if len(tt.comment) > 0 {
				commentBytes := make([]byte, record.CommentLength)
				buf.Read(commentBytes)
				if string(commentBytes) != tt.comment {
					t.Errorf("Comment mismatch: expected %s, got %s", tt.comment, string(commentBytes))
				}
			}

			// Verify disk numbers are zero (single disk archive)
			if record.ThisDiskNum != 0 {
				t.Errorf("thisDiskNum should be 0, got %d", record.ThisDiskNum)
			}
			if record.DiskNumWithTheStartOfCentralDir != 0 {
				t.Errorf("DiskNumWithTheStartOfCentralDir should be 0, got %d",
					record.DiskNumWithTheStartOfCentralDir)
			}
		})
	}
}

// TestSignatureConstants tests that all signature constants are correct
func TestSignatureConstants(t *testing.T) {
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
			// Convert signature to bytes and verify it starts with "PK"
			buf := make([]byte, 4)
			binary.LittleEndian.PutUint32(buf, tt.signature)

			if buf[0] != 'P' || buf[1] != 'K' {
				t.Errorf("Signature %x doesn't start with PK: got %s", tt.signature, string(buf[:2]))
			}

			// Verify the string representation
			actualString := string(buf)
			if actualString != tt.expected {
				t.Errorf("Signature string mismatch: expected %q, got %q", tt.expected, actualString)
			}
		})
	}
}

// TestTypesEdgeCases tests edge cases for structure encoding
func TestTypesEdgeCases(t *testing.T) {
	t.Run("Very long filename in central directory", func(t *testing.T) {
		longName := string(make([]byte, 255)) // Max typical filename length
		dir := centralDirectory{
			FilenameLength: uint16(len(longName)),
		}
		encoded := dir.encode(&file{name: longName})

		// Verify the encoded data includes the full filename
		if len(encoded) < 4+42+len(longName) { // signature + struct + filename
			t.Errorf("Encoded data too short for long filename")
		}
	})

	t.Run("Maximum values in end record", func(t *testing.T) {
		zip := &Zip{
			files: make([]*file, 65535), // Simulate max entries
		}
		// Use uint32 max values
		encoded := encodeEndOfCentralDirRecord(zip, 0xFFFFFFFF, 0xFFFFFFFF)

		var record endOfCentralDirectory
		buf := bytes.NewReader(encoded[4:]) // Skip signature
		binary.Read(buf, binary.LittleEndian, &record)

		if record.CentralDirSize != 0xFFFFFFFF {
			t.Errorf("Large centralDirSize not preserved")
		}
		if record.CentralDirOffset != 0xFFFFFFFF {
			t.Errorf("Large centralDirOffset not preserved")
		}
	})
}

// TestRoundTripEncoding tests encoding and partial decoding verification
func TestRoundTripEncoding(t *testing.T) {
	t.Run("Local file header round-trip", func(t *testing.T) {
		original := localFileHeader{
			VersionNeededToExtract: 20,
			GeneralPurposeBitFlag:  0x800, // UTF-8 filename flag
			CompressionMethod:      uint16(Deflated),
			LastModFileTime:        0x4D34,
			LastModFileDate:        0x5521,
			CRC32:                  0xDEADBEEF,
			CompressedSize:         12345,
			UncompressedSize:       67890,
			FilenameLength:         11,
			ExtraFieldLength:       0,
		}

		encoded := original.encode(&file{name: "test文件.txt"})

		// Decode and verify
		buf := bytes.NewReader(encoded[4:]) // Skip signature
		var decoded localFileHeader
		binary.Read(buf, binary.LittleEndian, &decoded)

		if decoded != original {
			t.Errorf("Round-trip failed: %+v != %+v", decoded, original)
		}
	})
}

// BenchmarkStructureEncoding benchmarks the performance of structure encoding
func BenchmarkStructureEncoding(b *testing.B) {
	header := localFileHeader{
		VersionNeededToExtract: 20,
		CompressionMethod:      uint16(Deflated),
		CRC32:                  0x12345678,
		CompressedSize:         1024,
		UncompressedSize:       2048,
		FilenameLength:         8,
	}
	filename := "test.txt"

	dir := centralDirectory{
		VersionMadeBy:          63,
		VersionNeededToExtract: 20,
		CompressionMethod:      uint16(Deflated),
		CRC32:                  0x12345678,
		CompressedSize:         1024,
		UncompressedSize:       2048,
		FilenameLength:         8,
		LocalHeaderOffset:      256,
	}

	zip := &Zip{
		files:   []*file{{name: filename}},
		comment: "benchmark",
	}

	b.ResetTimer()

	b.Run("LocalHeaderEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			header.encode(&file{name: filename})
		}
	})

	b.Run("CentralDirectoryEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			dir.encode(&file{name: filename})
		}
	})

	b.Run("EndOfCentralDirEncode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			encodeEndOfCentralDirRecord(zip, 1024, 2048)
		}
	})
}
