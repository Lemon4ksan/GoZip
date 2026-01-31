// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemon4ksan/gozip/internal/sys"
)

func TestNewFileFromPath(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")
	testContent := []byte("Auto clean content")

	if err := os.WriteFile(filePath, testContent, 0644); err != nil {
		t.Fatal(err)
	}

	f, err := newFileFromPath(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if f.Name() != "test.txt" {
		t.Errorf("Wrong name: %s", f.Name())
	}

	rc, err := f.Open()
	if err != nil {
		t.Fatalf("file.Open() failed: %v", err)
	}
	defer rc.Close()

	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read content failed: %v", err)
	}

	if string(content) != string(testContent) {
		t.Errorf("content mismatch: got %q, want %q", string(content), string(content))
	}
}

func TestNewFileFromOS(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "testfile_os")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		tmpfile.Close()
		os.Remove(tmpfile.Name())
	}()

	testContent := "OS File Content"
	if _, err := tmpfile.WriteString(testContent); err != nil {
		t.Fatal(err)
	}
	tmpfile.Sync()

	file, err := newFileFromOS(tmpfile)
	if err != nil {
		t.Fatalf("newFileFromOS failed: %v", err)
	}

	rc, err := file.Open()
	if err != nil {
		t.Fatalf("file.Open() failed: %v", err)
	}
	defer rc.Close()

	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read content failed: %v", err)
	}

	if string(content) != testContent {
		t.Errorf("content mismatch: got %q, want %q", string(content), testContent)
	}
}

func TestNewFileFromReader(t *testing.T) {
	testData := "test content"
	reader := strings.NewReader(testData)
	name := "test.txt"

	file, err := newFileFromReader(reader, name, SizeUnknown)
	if err != nil {
		t.Fatalf("NewFileFromReader failed: %v", err)
	}

	if file.Name() != name {
		t.Errorf("expected name %s, got %s", name, file.Name())
	}

	// Verify reading
	rc, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != testData {
		t.Errorf("content mismatch")
	}
}

func TestNewDirectoryFile(t *testing.T) {
	dirPath := "path/to"

	file, err := newDirectoryFile(path.Join(dirPath, "directory"))
	if err != nil {
		t.Fatalf("NewDirectoryFile failed: %v", err)
	}

	if !file.IsDir() {
		t.Error("expected file to be a directory")
	}

	// Directory size should be 0 (usually) or not matter, but mode should be correct
	if file.Mode()&os.ModeDir == 0 {
		t.Error("expected ModeDir bit set")
	}
}

func TestFileSetters(t *testing.T) {
	file := &File{}

	config := FileConfig{
		CompressionMethod: Deflate,
		CompressionLevel:  DeflateMaximum,
	}

	file.SetConfig(config)

	if file.config.CompressionMethod != Deflate {
		t.Error("compression method not set correctly")
	}
	if file.config.CompressionLevel != DeflateMaximum {
		t.Error("compression level not set correctly")
	}
}

func TestFile_ExtraFields(t *testing.T) {
	f := &File{name: "test"}

	// 1. Test Adding Fields
	tag1 := uint16(0xCAFE)
	data1 := make([]byte, 0, 6)
	binary.LittleEndian.PutUint16(data1[0:2], tag1)
	data1 = append(data1, []byte("coffee")...)

	if err := f.SetExtraField(tag1, data1); err != nil {
		t.Fatalf("SetExtraField failed: %v", err)
	}

	if !f.HasExtraField(tag1) {
		t.Error("HasExtraField returned false")
	}
	if !bytes.Equal(f.GetExtraField(tag1), data1) {
		t.Error("GetExtraField content mismatch")
	}

	// 2. Test Deterministic Sorting (Internal Helper)
	// Add another field with a LOWER tag ID to see if it comes first
	tag2 := uint16(0x0001) // Lower than 0xCAFE
	data2 := make([]byte, 2, 6)
	binary.LittleEndian.PutUint16(data2[0:2], tag2)
	data2 = append(data2, []byte("first")...)
	f.SetExtraField(tag2, data2)

	// Force parse/build via private method access (since we are in same package)
	headers := newZipHeaders(f)
	rawBytes := headers.buildExtraFieldBytes()

	// Parse manually to verify order
	// First field should be tag2 (0x0001)
	if len(rawBytes) < 4 {
		t.Fatal("Extra field bytes too short")
	}
	firstTag := binary.LittleEndian.Uint16(rawBytes[0:2])
	if firstTag != tag2 {
		t.Errorf("Extra fields not sorted. Expected first tag %x, got %x", tag2, firstTag)
	}
}

func TestFile_ExtraFieldLimit(t *testing.T) {
	f := &File{name: "limit_test"}

	// Create data larger than uint16 limit
	hugeData := make([]byte, 65536)

	err := f.SetExtraField(0x1234, hugeData)
	if !errors.Is(err, ErrExtraFieldTooLong) {
		t.Errorf("Expected ErrExtraFieldTooLong, got %v", err)
	}
}

func TestFile_ShouldCopyRaw(t *testing.T) {
	// Mock srcFunc (since it's private, we can set it in test inside same package)
	mockSrcFunc := func() (*io.SectionReader, error) { return nil, nil }

	tests := []struct {
		name      string
		srcConfig FileConfig
		dstConfig FileConfig
		hasSource bool
		want      bool
	}{
		{
			name:      "No Source",
			srcConfig: FileConfig{CompressionMethod: Deflate},
			dstConfig: FileConfig{CompressionMethod: Deflate},
			hasSource: false,
			want:      false, // No source reader available
		},
		{
			name:      "Perfect Match",
			srcConfig: FileConfig{CompressionMethod: Deflate, CompressionLevel: 5},
			dstConfig: FileConfig{CompressionMethod: Deflate, CompressionLevel: 5},
			hasSource: true,
			want:      true,
		},
		{
			name:      "Method Mismatch",
			srcConfig: FileConfig{CompressionMethod: Store},
			dstConfig: FileConfig{CompressionMethod: Deflate},
			hasSource: true,
			want:      false,
		},
		{
			name:      "Password Mismatch",
			srcConfig: FileConfig{EncryptionMethod: AES256, Password: "123"},
			dstConfig: FileConfig{EncryptionMethod: AES256, Password: "456"},
			hasSource: true,
			want:      false,
		},
		{
			name:      "Level Mismatch",
			srcConfig: FileConfig{CompressionMethod: Deflate, CompressionLevel: 5},
			dstConfig: FileConfig{CompressionMethod: Deflate, CompressionLevel: 9},
			hasSource: true,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{
				config:    tt.dstConfig,
				srcConfig: tt.srcConfig,
			}
			if tt.hasSource {
				f.srcFunc = mockSrcFunc
			}

			if got := f.shouldCopyRaw(); got != tt.want {
				t.Errorf("shouldCopyRaw() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZipHeaders_AttributesAndFlags(t *testing.T) {
	tests := []struct {
		name     string
		file     *File
		wantMode uint32 // Part of ExternalAttrs
		wantFlag uint16 // GeneralPurposeBitFlag
	}{
		{
			name: "Regular File 0644",
			file: &File{
				mode:       0644,
				hostSystem: sys.HostSystemUNIX,
			},
			wantMode: (sys.S_IFREG | 0644) << 16,
			wantFlag: 0x800, // UTF-8 bit
		},
		{
			name: "Executable 0755",
			file: &File{
				mode:       0755,
				hostSystem: sys.HostSystemUNIX,
			},
			wantMode: (sys.S_IFREG | 0755) << 16,
			wantFlag: 0x800,
		},
		{
			name: "Encrypted File",
			file: &File{
				config: FileConfig{EncryptionMethod: AES256, Password: "pwd"},
			},
			wantFlag: 0x800 | 0x1, // UTF-8 + Encryption bit
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newZipHeaders(tt.file)
			lh := h.LocalHeader()
			cd := h.CentralDirEntry()

			// Check Flags
			if lh.GeneralPurposeBitFlag&tt.wantFlag != tt.wantFlag {
				t.Errorf("BitFlag mismatch. Got %b, want mask %b", lh.GeneralPurposeBitFlag, tt.wantFlag)
			}

			// Check Attributes (Only checking logic for Unix here as configured)
			if tt.file.hostSystem == sys.HostSystemUNIX {
				if cd.ExternalFileAttributes != tt.wantMode {
					t.Errorf("ExternalAttrs mismatch. Got %x, want %x", cd.ExternalFileAttributes, tt.wantMode)
				}
			}
		})
	}
}

func TestZipHeaders(t *testing.T) {
	file := &File{
		name:             "test.txt",
		modTime:          time.Now(),
		uncompressedSize: 1024,
		config: FileConfig{
			CompressionMethod: Deflate,
		},
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	if localHeader.CompressionMethod != uint16(Deflate) {
		t.Error("compression method not set correctly")
	}

	centralDir := headers.CentralDirEntry()
	if centralDir.CompressedSize != 0 {
		t.Error("central dir compressed size should be 0 before compression")
	}
}

func TestZipHeaders_Directory(t *testing.T) {
	file := &File{
		name:  "archive/docs",
		isDir: true,
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	// "archive/docs" + "/" = 13 bytes
	expectedLength := uint16(len("archive/docs") + 1)
	if localHeader.FilenameLength != expectedLength {
		t.Errorf("directory filename length incorrect: got %d, expected %d",
			localHeader.FilenameLength, expectedLength)
	}

	if !strings.HasSuffix(localHeader.Filename, "/") {
		t.Error("directory filename in header missing trailing slash")
	}
}

func TestFile_RequiresZip64(t *testing.T) {
	tests := []struct {
		name     string
		file     *File
		expected bool
	}{
		{
			name: "Small file",
			file: &File{
				compressedSize:   100,
				uncompressedSize: 100,
			},
			expected: false,
		},
		{
			name: "Large uncompressed size",
			file: &File{
				compressedSize:   100,
				uncompressedSize: StandardSizeLimit + 1,
			},
			expected: true,
		},
		{
			name: "Large compressed size",
			file: &File{
				compressedSize:   StandardSizeLimit + 1,
				uncompressedSize: 100,
			},
			expected: true,
		},
		{
			name: "Large offset",
			file: &File{
				localHeaderOffset: StandardSizeLimit + 1,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.file.RequiresZip64()
			if result != tt.expected {
				t.Errorf("RequiresZip64() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestFile_GetFilenameLength(t *testing.T) {
	tests := []struct {
		name     string
		file     *File
		expected uint16
	}{
		{
			name: "Simple file",
			file: &File{
				name: "file.txt",
			},
			expected: 8,
		},
		{
			name: "File with path",
			file: &File{
				name: "path/to/file.txt",
			},
			expected: 16,
		},
		{
			name: "Directory",
			file: &File{
				name:  "archive/docs",
				isDir: true,
			},
			expected: 13, // +1 for slash
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length := uint16(len(tt.file.entryName()))
			if length != tt.expected {
				t.Errorf("getFilenameLength() = %d, expected %d", length, tt.expected)
			}
		})
	}
}

// TestIntegration_FileToHeaders verifies that file.go logic (getFilename) correctly propagates to internal structures
func TestIntegration_FileToHeaders(t *testing.T) {
	tests := []struct {
		name     string
		file     *File
		expected string // Expected string in the encoded bytes
	}{
		{
			name:     "Normal File",
			file:     &File{name: "doc.txt", isDir: false},
			expected: "doc.txt",
		},
		{
			name:     "Directory (Should have slash)",
			file:     &File{name: "images", isDir: true},
			expected: "images/",
		},
		{
			name:     "Nested Directory",
			file:     &File{name: "src/main", isDir: true},
			expected: "src/main/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. Create headers using the logic in file.go
			h := newZipHeaders(tt.file)

			// 2. Encode Local Header
			localEncoded := h.LocalHeader().Encode()

			// Verify Filename is present in bytes
			if !bytes.Contains(localEncoded, []byte(tt.expected)) {
				t.Errorf("Local Header bytes did not contain filename %q", tt.expected)
			}

			// Verify Filename Length field in bytes matches expected length
			// Length is at offset 26 in Local Header
			nameLen := binary.LittleEndian.Uint16(localEncoded[26:28])
			if int(nameLen) != len(tt.expected) {
				t.Errorf("Local Header name length: got %d, want %d", nameLen, len(tt.expected))
			}
		})
	}
}
