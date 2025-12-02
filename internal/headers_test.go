// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// Shadow structs for binary reading (excluding string/slice fields)
// These are necessary because binary.Read cannot handle string fields found in the main structs.
type rawLocalHeader struct {
	Signature              uint32
	VersionNeededToExtract uint16
	GeneralPurposeBitFlag  uint16
	CompressionMethod      uint16
	LastModFileTime        uint16
	LastModFileDate        uint16
	CRC32                  uint32
	CompressedSize         uint32
	UncompressedSize       uint32
	FilenameLength         uint16
	ExtraFieldLength       uint16
}

type rawCentralDirectory struct {
	Signature              uint32
	VersionMadeBy          uint16
	VersionNeededToExtract uint16
	GeneralPurposeBitFlag  uint16
	CompressionMethod      uint16
	LastModFileTime        uint16
	LastModFileDate        uint16
	CRC32                  uint32
	CompressedSize         uint32
	UncompressedSize       uint32
	FilenameLength         uint16
	ExtraFieldLength       uint16
	FileCommentLength      uint16
	DiskNumberStart        uint16
	InternalFileAttributes uint16
	ExternalFileAttributes uint32
	LocalHeaderOffset      uint32
}

// TestLocalFileHeader_Encode tests the updated encode method which now includes the filename
func TestLocalFileHeader_Encode(t *testing.T) {
	tests := []struct {
		name     string
		header   LocalFileHeader
		expected string // Expected filename in output
	}{
		{
			name: "Standard file",
			header: LocalFileHeader{
				VersionNeededToExtract: 20,
				CompressionMethod:      8,
				CRC32:                  0x12345678,
				CompressedSize:         100,
				UncompressedSize:       200,
				FilenameLength:         8,
				Filename:               "test.txt",
			},
			expected: "test.txt",
		},
		{
			name: "File inside directory",
			header: LocalFileHeader{
				VersionNeededToExtract: 20,
				CompressionMethod:      0,
				FilenameLength:         14,
				Filename:               "folder/doc.txt",
			},
			expected: "folder/doc.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Action
			encoded := tt.header.Encode()

			// Verification
			buf := bytes.NewReader(encoded)

			// 1. Verify Fixed Header
			var raw rawLocalHeader
			if err := binary.Read(buf, binary.LittleEndian, &raw); err != nil {
				t.Fatalf("Failed to read raw header: %v", err)
			}

			if raw.Signature != LocalFileHeaderSignature {
				t.Errorf("Signature mismatch: got %x, want %x", raw.Signature, LocalFileHeaderSignature)
			}
			if raw.FilenameLength != tt.header.FilenameLength {
				t.Errorf("FilenameLength mismatch: got %d, want %d", raw.FilenameLength, tt.header.FilenameLength)
			}

			// 2. Verify Variable Data (Filename)
			filenameBytes := make([]byte, raw.FilenameLength)
			if _, err := io.ReadFull(buf, filenameBytes); err != nil {
				t.Fatalf("Failed to read filename from buffer: %v", err)
			}

			if string(filenameBytes) != tt.expected {
				t.Errorf("Filename mismatch: got %q, want %q", string(filenameBytes), tt.expected)
			}

			// 3. Check total size matches expectations
			expectedSize := 30 + int(tt.header.FilenameLength) + int(tt.header.ExtraFieldLength)
			if len(encoded) != expectedSize {
				t.Errorf("Total encoded size mismatch: got %d, want %d", len(encoded), expectedSize)
			}
		})
	}
}

// TestCentralDirectory_Encode tests the updated encode method with Filename, ExtraFields, and Comment
func TestCentralDirectory_Encode(t *testing.T) {
	extraData := []byte{0x01, 0x02, 0x03} // Fake extra data

	tests := []struct {
		name             string
		entry            CentralDirectory
		expectedFilename string
		expectedComment  string
	}{
		{
			name: "Simple Entry",
			entry: CentralDirectory{
				VersionMadeBy:     63,
				CRC32:             0xAABBCCDD,
				FilenameLength:    8,
				Filename:          "test.txt",
				LocalHeaderOffset: 12345,
			},
			expectedFilename: "test.txt",
			expectedComment:  "",
		},
		{
			name: "Entry with Extra Field and Comment",
			entry: CentralDirectory{
				VersionMadeBy:     63,
				FilenameLength:    9,
				ExtraFieldLength:  3,
				FileCommentLength: 13,
				Filename:          "image.png",
				ExtraField:        map[uint16][]byte{0xaaaa: extraData},
				Comment:           "Hello Archive",
			},
			expectedFilename: "image.png",
			expectedComment:  "Hello Archive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Action
			encoded := tt.entry.Encode()

			// Verification
			buf := bytes.NewReader(encoded)

			// 1. Verify Fixed Header
			var raw rawCentralDirectory
			if err := binary.Read(buf, binary.LittleEndian, &raw); err != nil {
				t.Fatalf("Failed to read raw central dir: %v", err)
			}

			if raw.Signature != CentralDirectorySignature {
				t.Errorf("Signature mismatch: got %x, want %x", raw.Signature, CentralDirectorySignature)
			}

			// 2. Verify Filename
			filenameBytes := make([]byte, raw.FilenameLength)
			if _, err := io.ReadFull(buf, filenameBytes); err != nil {
				t.Fatalf("Reading filename: %v", err)
			}
			if string(filenameBytes) != tt.expectedFilename {
				t.Errorf("Filename mismatch: got %q, want %q", string(filenameBytes), tt.expectedFilename)
			}

			// 3. Verify Extra Fields
			if raw.ExtraFieldLength > 0 {
				extraBytes := make([]byte, raw.ExtraFieldLength)
				if _, err := io.ReadFull(buf, extraBytes); err != nil {
					t.Fatalf("Reading extra fields: %v", err)
				}
				if !bytes.Equal(extraBytes, extraData) {
					t.Error("Extra field data mismatch")
				}
			}

			// 4. Verify Comment
			if raw.FileCommentLength > 0 {
				commentBytes := make([]byte, raw.FileCommentLength)
				if _, err := io.ReadFull(buf, commentBytes); err != nil {
					t.Fatalf("Reading comment: %v", err)
				}
				if string(commentBytes) != tt.expectedComment {
					t.Errorf("Comment mismatch: got %q, want %q", string(commentBytes), tt.expectedComment)
				}
			}
		})
	}
}

// TestEndOfCentralDir_Encode tests the EOCD record encoding including the comment
func TestEndOfCentralDir_Encode(t *testing.T) {
	entries := 5
	size := uint64(1024)
	offset := uint64(2048)
	comment := "End of Archive"

	// Action
	encoded := EncodeEndOfCentralDirRecord(entries, size, offset, comment)

	// Verification
	if len(encoded) != 22+len(comment) {
		t.Errorf("Encoded length mismatch: got %d, want %d", len(encoded), 22+len(comment))
	}

	buf := bytes.NewReader(encoded)

	// Check Signature
	var signature uint32
	binary.Read(buf, binary.LittleEndian, &signature)
	if signature != EndOfCentralDirSignature {
		t.Errorf("Signature mismatch")
	}

	// Skip to Comment Length (Offset 20)
	buf.Seek(20, io.SeekStart)
	var commentLen uint16
	binary.Read(buf, binary.LittleEndian, &commentLen)

	if int(commentLen) != len(comment) {
		t.Errorf("Comment length mismatch: got %d, want %d", commentLen, len(comment))
	}

	// Verify Comment Body
	actualComment := make([]byte, commentLen)
	io.ReadFull(buf, actualComment)
	if string(actualComment) != comment {
		t.Errorf("Comment content mismatch: got %q, want %q", string(actualComment), comment)
	}
}

// TestZip64Records tests the structure of Zip64 specific records
func TestZip64Records(t *testing.T) {
	t.Run("Zip64 End Of Central Directory", func(t *testing.T) {
		encoded := EncodeZip64EndOfCentralDirRecord(100, 5000, 10000)

		if len(encoded) != 56 {
			t.Errorf("Zip64 EOCD size mismatch: got %d, want 56", len(encoded))
		}

		sig := binary.LittleEndian.Uint32(encoded[0:4])
		if sig != Zip64EndOfCentralDirSignature {
			t.Errorf("Signature mismatch")
		}

		sizeOfRest := binary.LittleEndian.Uint64(encoded[4:12])
		if sizeOfRest != 44 {
			t.Errorf("Size of rest mismatch: got %d, want 44", sizeOfRest)
		}
	})

	t.Run("Zip64 Locator", func(t *testing.T) {
		encoded := EncodeZip64EndOfCentralDirLocator(9999)

		if len(encoded) != 20 {
			t.Errorf("Zip64 Locator size mismatch: got %d, want 20", len(encoded))
		}

		sig := binary.LittleEndian.Uint32(encoded[0:4])
		if sig != Zip64EndOfCentralDirLocatorSignature {
			t.Errorf("Signature mismatch")
		}
	})
}
