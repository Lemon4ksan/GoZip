package gozip

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewFileFromPath(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	testContent := "Hello, World!"
	if _, err := tmpfile.WriteString(testContent); err != nil {
		t.Fatal(err)
	}
	tmpfile.Sync()

	file, err := newFileFromPath(tmpfile.Name())
	if err != nil {
		t.Fatalf("newFileFromPath failed: %v", err)
	}

	if file.Name() != filepath.Base(tmpfile.Name()) {
		t.Errorf("expected name %s, got %s", filepath.Base(tmpfile.Name()), file.Name())
	}

	if file.UncompressedSize() != int64(len(testContent)) {
		t.Errorf("expected size %d, got %d", len(testContent), file.UncompressedSize())
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
}

func TestFileSetters(t *testing.T) {
	file := &file{}

	config := FileConfig{
		CompressionMethod: Deflated,
		CompressionLevel:  DeflateMaximum,
	}

	file.SetConfig(config)

	if file.config.CompressionMethod != Deflated {
		t.Error("compression method not set correctly")
	}
}

func TestZipHeaders(t *testing.T) {
	file := &file{
		name:             "test.txt",
		modTime:          time.Now(),
		uncompressedSize: 1024,
		config: FileConfig{
			CompressionMethod: Deflated,
		},
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	if localHeader.CompressionMethod != uint16(Deflated) {
		t.Error("compression method not set correctly")
	}

	centralDir := headers.CentralDirEntry()
	if centralDir.CompressedSize != 0 {
		t.Error("central dir compressed size should be 0 before compression")
	}
}

func TestZipHeaders_Directory(t *testing.T) {
	file := &file{
		name:  "docs",
		path:  "archive",
		isDir: true,
	}

	headers := newZipHeaders(file)
	localHeader := headers.LocalHeader()

	expectedLength := uint16(len("archive/docs") + 1)
	if localHeader.FilenameLength != expectedLength {
		t.Errorf("directory filename length incorrect: got %d, expected %d",
			localHeader.FilenameLength, expectedLength)
	}
}

func TestFileMetadata_ExtraFields(t *testing.T) {
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
}

func TestFileMetadata_ExtraFieldLength(t *testing.T) {
	file := &file{
		extraField: []ExtraFieldEntry{
			{Tag: 0x0001, Data: []byte{0x01, 0x02, 0x03}},
			{Tag: 0x0002, Data: []byte{0x04, 0x05}},
		},
	}

	expected := uint16(5)
	actual := file.getExtraFieldLength()

	if actual != expected {
		t.Errorf("extra field length incorrect: got %d, expected %d", actual, expected)
	}
}

func TestFileAttributes_Version(t *testing.T) {
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
				isDir: true,
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
		{
			name: "Zip64 file",
			file: &file{
				uncompressedSize: math.MaxUint32 + 1,
			},
			expected: 45,
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

func TestFileAttributes_BitFlags(t *testing.T) {
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
				},
			},
			expected: 0x0001,
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

func TestFileAttributes_ExternalAttributes(t *testing.T) {
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
			name: "NTFS file",
			file: &file{
				hostSystem: HostSystemNTFS,
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

func TestFile_RequiresZip64(t *testing.T) {
	tests := []struct {
		name     string
		file     *file
		expected bool
	}{
		{
			name: "Small file",
			file: &file{
				compressedSize:   100,
				uncompressedSize: 100,
			},
			expected: false,
		},
		{
			name: "Large uncompressed size",
			file: &file{
				compressedSize:   100,
				uncompressedSize: math.MaxUint32 + 1,
			},
			expected: true,
		},
		{
			name: "Large compressed size",
			file: &file{
				compressedSize:   math.MaxUint32 + 1,
				uncompressedSize: 100,
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
		file     *file
		expected uint16
	}{
		{
			name: "Simple file",
			file: &file{
				name: "file.txt",
				path: "",
			},
			expected: 8,
		},
		{
			name: "File with path",
			file: &file{
				name: "file.txt",
				path: "path/to",
			},
			expected: 16,
		},
		{
			name: "Directory",
			file: &file{
				name:  "docs",
				path:  "archive",
				isDir: true,
			},
			expected: 13,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length := tt.file.getFilenameLength()
			if length != tt.expected {
				t.Errorf("getFilenameLength() = %d, expected %d", length, tt.expected)
			}
		})
	}
}
