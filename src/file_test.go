package gozip

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConstants(t *testing.T) {
	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{"LATEST_ZIP_VERSION", __LATEST_ZIP_VERSION, uint16(63)},
		{"ZIP64_EXTRA_FIELD", __ZIP64_EXTRA_FIELD, uint16(0x001)},
		{"NTFS_METADATA_FIELD", __NTFS_METADATA_FIELD, uint16(0x00A)},
		{"Stored compression", Stored, CompressionMethod(0)},
		{"Deflated compression", Deflated, CompressionMethod(8)},
		{"HostSystemFAT", HostSystemFAT, HostSystem(0)},
		{"HostSystemUNIX", HostSystemUNIX, HostSystem(3)},
		{"HostSystemNTFS", HostSystemNTFS, HostSystem(10)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("got %v, expected %v", tt.got, tt.expected)
			}
		})
	}
}

func TestCompressionLevels(t *testing.T) {
	levels := map[string]int{
		"DeflateSuperFast": DeflateSuperFast,
		"DeflateFast":      DeflateFast,
		"DeflateNormal":    DeflateNormal,
		"DeflateMaximum":   DeflateMaximum,
	}

	for name, level := range levels {
		t.Run(name, func(t *testing.T) {
			if level < -2 || level > 9 {
				t.Errorf("invalid compression level %d for %s", level, name)
			}
		})
	}
}

func TestNewFileFromOS(t *testing.T) {
	// Create temporary file for testing
	tmpfile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())
	defer tmpfile.Close()

	testContent := "Hello, World!"
	if _, err := tmpfile.WriteString(testContent); err != nil {
		t.Fatal(err)
	}
	tmpfile.Sync()

	file, err := newFileFromOS(tmpfile)
	if err != nil {
		t.Fatalf("newFileFromOS failed: %v", err)
	}

	if file.Name() != filepath.Base(tmpfile.Name()) {
		t.Errorf("expected name %s, got %s", filepath.Base(tmpfile.Name()), file.Name())
	}

	if file.UncompressedSize() != int64(len(testContent)) {
		t.Errorf("expected size %d, got %d", len(testContent), file.UncompressedSize())
	}

	if file.IsDir() {
		t.Error("expected file to not be a directory")
	}
}

func TestNewFileFromReader(t *testing.T) {
	reader := strings.NewReader("test content")
	name := "test.txt"

	file, err := newFileFromReader(reader, name)
	if err != nil {
		t.Fatalf("NewFileFromReader failed: %v", err)
	}

	if file.Name() != name {
		t.Errorf("expected name %s, got %s", name, file.Name())
	}

	// Should be created recently
	if time.Since(file.ModTime()) > time.Second {
		t.Error("file mod time should be recent")
	}
}

func TestNewDirectoryFile(t *testing.T) {
	dirPath := "path/to"
	dirName := "directory"

	file, err := newDirectoryFile(dirPath, dirName)
	if err != nil {
		t.Fatalf("NewDirectoryFile failed: %v", err)
	}

	if !file.IsDir() {
		t.Error("expected file to be a directory")
	}

	if file.Path() != dirPath {
		t.Errorf("expected path %s, got %s", dirPath, file.Path())
	}

	if file.Name() != dirName {
		t.Errorf("expected name %s, got %s", dirName, file.Name())
	}
}

func TestFileSettersAndGetters(t *testing.T) {
	file := &file{}

	config := FileConfig{
		CompressionMethod: Deflated,
		CompressionLevel:  DeflateMaximum,
		Comment:           "test comment",
	}

	file.SetConfig(config)
	file.SetSource(strings.NewReader("test"))

	if file.config.CompressionMethod != Deflated {
		t.Error("compression method not set correctly")
	}
}

func TestFileOperations_CompressAndWrite_Stored(t *testing.T) {
	content := "Hello, World! This is test content for compression."
	reader := strings.NewReader(content)

	file, err := newFileFromReader(reader, "test.txt")
	if err != nil {
		t.Fatal(err)
	}

	file.SetConfig(FileConfig{
		CompressionMethod: Stored,
	})

	ops := NewFileOperations(file)
	var buf bytes.Buffer

	err = ops.CompressAndWrite(&buf)
	if err != nil {
		t.Fatalf("CompressAndWrite failed: %v", err)
	}

	if file.UncompressedSize() != int64(len(content)) {
		t.Errorf("expected uncompressed size %d, got %d", len(content), file.UncompressedSize())
	}

	if file.CompressedSize() != int64(len(content)) {
		t.Errorf("for stored compression, compressed size should equal uncompressed size")
	}

	if file.CRC32() == 0 {
		t.Error("CRC32 should be calculated")
	}

	// Verify the written content matches original
	if buf.String() != content {
		t.Error("stored content doesn't match original")
	}
}

func TestFileOperations_CompressAndWrite_Deflated(t *testing.T) {
	content := strings.Repeat("Hello, Deflated World! ", 10)
	reader := strings.NewReader(content)

	file, err := newFileFromReader(reader, "test.txt")
	if err != nil {
		t.Fatal(err)
	}

	file.SetConfig(FileConfig{
		CompressionMethod: Deflated,
		CompressionLevel:  DeflateNormal,
	})

	ops := NewFileOperations(file)
	var buf bytes.Buffer

	err = ops.CompressAndWrite(&buf)
	if err != nil {
		t.Fatalf("CompressAndWrite failed: %v", err)
	}

	if file.UncompressedSize() != int64(len(content)) {
		t.Errorf("expected uncompressed size %d, got %d", len(content), file.UncompressedSize())
	}

	// Deflated content should be smaller than original
	if file.CompressedSize() >= file.UncompressedSize() {
		t.Error("deflated content should be compressed")
	}

	if file.CRC32() == 0 {
		t.Error("CRC32 should be calculated")
	}
}

func TestFileOperations_EmptyFile(t *testing.T) {
	reader := strings.NewReader("")
	file, err := newFileFromReader(reader, "empty.txt")
	if err != nil {
		t.Fatal(err)
	}

	file.SetConfig(FileConfig{CompressionMethod: Stored})

	ops := NewFileOperations(file)
	var buf bytes.Buffer

	err = ops.CompressAndWrite(&buf)
	if err != nil {
		t.Fatalf("CompressAndWrite failed for empty file: %v", err)
	}

	if file.UncompressedSize() != 0 {
		t.Error("empty file should have zero size")
	}

	if file.CRC32() != 0 {
		t.Errorf("empty file CRC32 should be 0, got %d", file.CRC32())
	}
}

func TestZipHeaders_LocalHeader(t *testing.T) {
	file := &file{
		name:             "test.txt",
		modTime:          time.Now(),
		uncompressedSize: 1024,
		config: FileConfig{
			CompressionMethod: Deflated,
			CompressionLevel:  DeflateNormal,
		},
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	if localHeader.VersionNeededToExtract != 20 {
		t.Errorf("expected version 20, got %d", localHeader.VersionNeededToExtract)
	}

	if localHeader.CompressionMethod != uint16(Deflated) {
		t.Error("compression method not set correctly")
	}

	if localHeader.FilenameLength != uint16(len("test.txt")) {
		t.Errorf("filename length incorrect: got %d", localHeader.FilenameLength)
	}
}

func TestZipHeaders_CentralDirEntry(t *testing.T) {
	file := &file{
		name:              "document.pdf",
		path:              "files",
		modTime:           time.Date(2023, 12, 1, 14, 30, 0, 0, time.UTC),
		uncompressedSize:  2048,
		compressedSize:    1024,
		crc32:             0x12345678,
		localHeaderOffset: 512,
		config: FileConfig{
			CompressionMethod: Stored,
			Comment:           "test file",
		},
	}

	headers := newZipHeaders(file)
	centralDir := headers.CentralDirEntry()

	if centralDir.CRC32 != file.crc32 {
		t.Errorf("CRC32 mismatch: got %x, expected %x", centralDir.CRC32, file.crc32)
	}

	if centralDir.CompressedSize != uint32(file.compressedSize) {
		t.Errorf("compressed size mismatch: got %d, expected %d",
			centralDir.CompressedSize, file.compressedSize)
	}

	if centralDir.LocalHeaderOffset != uint32(file.localHeaderOffset) {
		t.Errorf("local header offset mismatch: got %d, expected %d",
			centralDir.LocalHeaderOffset, file.localHeaderOffset)
	}

	if centralDir.FileCommentLength != uint16(len("test file")) {
		t.Errorf("comment length incorrect: got %d", centralDir.FileCommentLength)
	}
}

func TestZipHeaders_Directory(t *testing.T) {
	file := &file{
		name:    "docs",
		path:    "archive",
		isDir:   true,
		modTime: time.Now(),
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	// Directory should have path included in filename length
	expectedLength := uint16(len("archive/docs") + 1) // +1 for trailing slash
	if localHeader.FilenameLength != expectedLength {
		t.Errorf("directory filename length incorrect: got %d, expected %d",
			localHeader.FilenameLength, expectedLength)
	}
}

func TestFileMetadata_AddExtraFieldEntry(t *testing.T) {
	file := &file{}
	metadata := NewFileMetadata(file)

	entry := ExtraFieldEntry{
		Tag:  0x1234,
		Data: []byte{0x01, 0x02, 0x03, 0x04},
	}

	err := metadata.AddExtraFieldEntry(entry)
	if err != nil {
		t.Fatalf("AddExtraFieldEntry failed: %v", err)
	}

	if !metadata.HasExtraField(0x1234) {
		t.Error("extra field was not added")
	}

	// Test duplicate tag
	err = metadata.AddExtraFieldEntry(entry)
	if err == nil {
		t.Error("expected error for duplicate tag")
	}
}

func TestFileMetadata_HasExtraField(t *testing.T) {
	file := &file{
		extraField: []ExtraFieldEntry{
			{Tag: 0x1111, Data: []byte{0x01}},
			{Tag: 0x2222, Data: []byte{0x02}},
		},
	}
	metadata := NewFileMetadata(file)

	if !metadata.HasExtraField(0x1111) {
		t.Error("HasExtraField should find existing tag")
	}

	if metadata.HasExtraField(0x9999) {
		t.Error("HasExtraField should not find non-existent tag")
	}
}

func TestFileMetadata_GetExtraFieldLength(t *testing.T) {
	file := &file{
		extraField: []ExtraFieldEntry{
			{Tag: 0x0001, Data: []byte{0x01, 0x02, 0x03}},
			{Tag: 0x0002, Data: []byte{0x04, 0x05}},
		},
	}

	expected := uint16(5) // 3 + 2 bytes
	actual := file.getExtraFieldLength()

	if actual != expected {
		t.Errorf("extra field length incorrect: got %d, expected %d", actual, expected)
	}
}

func TestFileMetadata_UpdateFileMetadataExtraField(t *testing.T) {
	file := &file{
		hostSystem: HostSystemNTFS,
		metadata: map[string]interface{}{
			"LastWriteTime":  uint64(133052840000000000),
			"LastAccessTime": uint64(133052840000000000),
			"CreationTime":   uint64(133052840000000000),
		},
	}

	metadata := NewFileMetadata(file)
	metadata.SetupFileMetadataExtraField()

	if !metadata.HasExtraField(__NTFS_METADATA_FIELD) {
		t.Error("NTFS metadata extra field should be added")
	}

	// Test with nil metadata
	file.metadata = nil
	file.extraField = nil
	metadata.SetupFileMetadataExtraField()

	if len(file.extraField) != 0 {
		t.Error("no extra fields should be added with nil metadata")
	}
}

func TestFileAttributes_GetVersionNeededToExtract(t *testing.T) {
	tests := []struct {
		name     string
		file     *file
		expected uint16
	}{
		{
			name: "Regular file stored",
			file: &file{
				config: FileConfig{CompressionMethod: Stored},
			},
			expected: 10,
		},
		{
			name: "Directory",
			file: &file{
				isDir:  true,
				config: FileConfig{CompressionMethod: Stored},
			},
			expected: 20,
		},
		{
			name: "File with path",
			file: &file{
				path:   "subdir",
				config: FileConfig{CompressionMethod: Stored},
			},
			expected: 20,
		},
		{
			name: "Deflated file",
			file: &file{
				config: FileConfig{CompressionMethod: Deflated},
			},
			expected: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := NewFileAttributes(tt.file)
			version := attrs.GetVersionNeededToExtract()
			if version != tt.expected {
				t.Errorf("got version %d, expected %d", version, tt.expected)
			}
		})
	}
}

func TestFileAttributes_GetVersionMadeBy(t *testing.T) {
	file := &file{
		hostSystem: HostSystemUNIX,
	}

	attrs := NewFileAttributes(file)
	version := attrs.GetVersionMadeBy()

	expected := uint16(HostSystemUNIX)<<8 | __LATEST_ZIP_VERSION
	if version != expected {
		t.Errorf("got version made by %x, expected %x", version, expected)
	}
}

func TestFileAttributes_GetFileBitFlag(t *testing.T) {
	tests := []struct {
		name     string
		file     *file
		expected uint16
	}{
		{
			name: "Encrypted deflated",
			file: &file{
				config: FileConfig{
					CompressionMethod: Deflated,
					IsEncrypted:       true,
					CompressionLevel:  DeflateNormal,
				},
			},
			expected: 0x0001, // Only encryption bit
		},
		{
			name: "Deflated maximum",
			file: &file{
				config: FileConfig{
					CompressionMethod: Deflated,
					CompressionLevel:  DeflateMaximum,
				},
			},
			expected: 0x0002,
		},
		{
			name: "Deflated fast",
			file: &file{
				config: FileConfig{
					CompressionMethod: Deflated,
					CompressionLevel:  DeflateFast,
				},
			},
			expected: 0x0004,
		},
		{
			name: "Stored no flags",
			file: &file{
				config: FileConfig{
					CompressionMethod: Stored,
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := NewFileAttributes(tt.file)
			flags := attrs.GetFileBitFlag()
			if flags != tt.expected {
				t.Errorf("got flags %04x, expected %04x", flags, tt.expected)
			}
		})
	}
}

func TestFileAttributes_GetExternalFileAttributes(t *testing.T) {
	tests := []struct {
		name     string
		file     *file
		expected uint32
	}{
		{
			name: "FAT directory",
			file: &file{
				hostSystem: HostSystemFAT,
				isDir:      true,
			},
			expected: 0x10,
		},
		{
			name: "FAT file",
			file: &file{
				hostSystem: HostSystemFAT,
				isDir:      false,
			},
			expected: 0x20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := NewFileAttributes(tt.file)
			attributes := attrs.GetExternalFileAttributes()
			if attributes != tt.expected {
				t.Errorf("got attributes %x, expected %x", attributes, tt.expected)
			}
		})
	}
}
