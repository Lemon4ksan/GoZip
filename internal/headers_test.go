// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// TestLocalFileHeader_RoundTrip verifies that data remains consistent
// after Encoding and then Reading back.
func TestLocalFileHeader_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   LocalFileHeader
	}{
		{
			name: "Basic",
			in: LocalFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0x800,
				CompressionMethod:      8,
				LastModFileTime:        0x4B00,
				LastModFileDate:        0x5600,
				CRC32:                  0xAABBCCDD,
				CompressedSize:         500,
				UncompressedSize:       1000,
				FilenameLength:         8,
				ExtraFieldLength:       4,
				Filename:               "test.txt",
				ExtraField:             []byte{0xDE, 0xAD, 0xBE, 0xEF},
			},
		},
		{
			name: "Empty Filename and Extra",
			in: LocalFileHeader{
				VersionNeededToExtract: 10,
				CompressionMethod:      0,
			},
		},
		{
			name: "UTF-8 Filename",
			in: LocalFileHeader{
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0x800,
				FilenameLength:         16, // bytes length
				Filename:               "привет.txt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.in.Encode()

			reader := bytes.NewReader(encoded)

			var sig uint32
			binary.Read(reader, binary.LittleEndian, &sig)
			if sig != LocalFileHeaderSignature {
				t.Fatal("Wrong signature encoded")
			}

			out, err := ReadLocalFileHeader(reader)
			if err != nil {
				t.Fatalf("ReadLocalFileHeader failed: %v", err)
			}

			if !reflect.DeepEqual(tt.in, out) {
				t.Errorf("RoundTrip mismatch.\nIn:  %+v\nOut: %+v", tt.in, out)
			}
		})
	}
}

// TestCentralDirectory_RoundTrip verifies consistency for Central Directory entries.
func TestCentralDirectory_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   CentralDirectory
	}{
		{
			name: "Full Entry",
			in: CentralDirectory{
				VersionMadeBy:          63,
				VersionNeededToExtract: 20,
				GeneralPurposeBitFlag:  0,
				CompressionMethod:      8,
				CRC32:                  0x12345678,
				CompressedSize:         1024,
				UncompressedSize:       2048,
				FilenameLength:         9,
				ExtraFieldLength:       4,
				FileCommentLength:      5,
				LocalHeaderOffset:      12345,
				Filename:               "image.png",
				ExtraField:             []byte{1, 2, 3, 4},
				Comment:                "hello",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.in.Encode()

			reader := bytes.NewReader(encoded)

			var sig uint32
			binary.Read(reader, binary.LittleEndian, &sig)
			if sig != CentralDirectorySignature {
				t.Fatal("Wrong signature encoded")
			}

			out, err := ReadCentralDirEntry(reader)
			if err != nil {
				t.Fatalf("ReadCentralDirEntry failed: %v", err)
			}

			if !reflect.DeepEqual(tt.in, out) {
				t.Errorf("Mismatch.\nIn:  %+v\nOut: %+v", tt.in, out)
			}
		})
	}
}

// TestEOCD_RoundTrip verifies End of Central Directory Record.
func TestEOCD_RoundTrip(t *testing.T) {
	entries := 15
	size := int64(3000)
	offset := int64(5000)
	comment := "Archive Comment"

	encoded := EncodeEOCD(entries, size, offset, comment)

	reader := bytes.NewReader(encoded)

	var sig uint32
	binary.Read(reader, binary.LittleEndian, &sig)
	if sig != EOCDSignature {
		t.Fatal("Wrong signature encoded")
	}

	eocd, err := ReadEOCD(reader)
	if err != nil {
		t.Fatalf("ReadEOCD failed: %v", err)
	}

	if int(eocd.EntriesNum) != entries {
		t.Errorf("Entries mismatch: got %d, want %d", eocd.EntriesNum, entries)
	}
	if int64(eocd.CentralDirSize) != size {
		t.Errorf("Size mismatch: got %d, want %d", eocd.CentralDirSize, size)
	}
	if int64(eocd.CentralDirOffset) != offset {
		t.Errorf("Offset mismatch: got %d, want %d", eocd.CentralDirOffset, offset)
	}
	if eocd.Comment != comment {
		t.Errorf("Comment mismatch: got %q, want %q", eocd.Comment, comment)
	}
}

func TestEOCD_Limits(t *testing.T) {
	// Test clamping logic (e.g. entries > 65535 should become 65535 in standard EOCD)
	hugeEntries := 100000
	encoded := EncodeEOCD(hugeEntries, 0, 0, "")

	reader := bytes.NewReader(encoded)

	var sig uint32
	binary.Read(reader, binary.LittleEndian, &sig)
	if sig != EOCDSignature {
		t.Fatal("Wrong signature encoded")
	}

	eocd, _ := ReadEOCD(reader)

	if eocd.EntriesNum != 0xFFFF {
		t.Errorf("Expected clamping to 0xFFFF, got %d", eocd.EntriesNum)
	}
}

func TestZip64EOCD_RoundTrip(t *testing.T) {
	entries := 100000 // More than uint16
	size := int64(5000000000)
	offset := int64(9000000000)

	encoded := EncodeZip64EOCDRecord(entries, size, offset)

	reader := bytes.NewReader(encoded)

	var sig uint32
	binary.Read(reader, binary.LittleEndian, &sig)
	if sig != Zip64EOCDSignature {
		t.Fatal("Wrong signature encoded")
	}

	out, err := ReadZip64EOCD(reader)
	if err != nil {
		t.Fatal(err)
	}

	if int(out.EntriesNum) != entries {
		t.Errorf("Entries mismatch: got %d, want %d", out.EntriesNum, entries)
	}
	if int64(out.CentralDirOffset) != offset {
		t.Errorf("Offset mismatch")
	}
}

func TestZip64Locator_RoundTrip(t *testing.T) {
	offset := int64(123456789)
	encoded := EncodeZip64EOCDLocator(offset)

	reader := bytes.NewReader(encoded)

	var sig uint32
	binary.Read(reader, binary.LittleEndian, &sig)
	if sig != Zip64EOCDLocatorSignature {
		t.Fatal("Wrong signature encoded")
	}

	out, err := ReadZip64EOCDLocator(reader)
	if err != nil {
		t.Fatal(err)
	}

	if int64(out.Zip64EndOfCentralDirOffset) != offset {
		t.Errorf("Offset mismatch: got %d, want %d", out.Zip64EndOfCentralDirOffset, offset)
	}
}

func TestExtraFields_Parsing(t *testing.T) {
	// Construct raw bytes: Tag(2) + Size(2) + Data
	// Field 1: Tag=0x0001 (Zip64), Size=4, Data=0xDEADBEEF
	// Field 2: Tag=0xCAFE, Size=2, Data=0xBEAF
	raw := []byte{
		0x01, 0x00, 0x04, 0x00, 0xEF, 0xBE, 0xAD, 0xDE,
		0xFE, 0xCA, 0x02, 0x00, 0xAF, 0xBE,
	}

	parsed := ParseExtraField(raw)

	if len(parsed) != 2 {
		t.Fatalf("Expected 2 fields, got %d", len(parsed))
	}

	// Check Zip64
	if val, ok := parsed[0x0001]; !ok {
		t.Error("Missing Zip64 tag")
	} else if !bytes.Equal(val, []byte{0x01, 0x00, 0x04, 0x00, 0xEF, 0xBE, 0xAD, 0xDE}) {
		t.Error("Wrong data for Zip64 tag")
	}

	// Check Custom
	if val, ok := parsed[0xCAFE]; !ok {
		t.Error("Missing CAFE tag")
	} else if !bytes.Equal(val, []byte{0xFE, 0xCA, 0x02, 0x00, 0xAF, 0xBE}) {
		t.Error("Wrong data for CAFE tag")
	}
}
