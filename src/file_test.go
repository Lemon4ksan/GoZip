package gozip

import (
	"math"
	"os"
	"path"
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

	file, err := newDirectoryFile(path.Join(dirPath, "directory"))
	if err != nil {
		t.Fatalf("NewDirectoryFile failed: %v", err)
	}

	if !file.IsDir() {
		t.Error("expected file to be a directory")
	}
}

func TestFileSetters(t *testing.T) {
	file := &File{}

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
	file := &File{
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
	file := &File{
		name:  "archive/docs",
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
				uncompressedSize: math.MaxUint32 + 1,
			},
			expected: true,
		},
		{
			name: "Large compressed size",
			file: &File{
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
			expected: 13,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length := uint16(len(tt.file.getFilename()))
			if length != tt.expected {
				t.Errorf("getFilenameLength() = %d, expected %d", length, tt.expected)
			}
		})
	}
}
