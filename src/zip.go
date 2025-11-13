package gozip

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// FileConfig holds configuration for file processing
type FileConfig struct {
	CompressionMethod CompressionMethod
	CompressionLevel  int
	Comment           string
	IsEncrypted       bool
	Path              string
}

// AddOption defines a function type for configuring file options during addition to archive
type AddOption func(f *file)

func WithConfig(c FileConfig) AddOption {
	return func(f *file) {
		f.SetConfig(c)
	}
}

// Zip represents an editable ZIP archive in memory.
// Provides methods to add files, configure compression/encryption, and save the archive.
type Zip struct {
	compressionMethod CompressionMethod // Default compression for all files
	encryptionMethod  EncryptionMethod  // Encryption method for the archive
	password          string            // Password for encrypted archives
	files             []*file           // List of files in the archive
	comment           string            // Global archive comment
}

// NewZip creates a new empty ZIP archive with the specified default compression method.
// The compression method can be overridden per-file using [AddOption].
func NewZip(c CompressionMethod) *Zip {
	return &Zip{
		compressionMethod: c,
	}
}

// SetComment sets a global comment for the entire ZIP archive.
// The comment is stored in the end of central directory record.
func (z *Zip) SetComment(comment string) {
	z.comment = comment
}

// SetEncryption configures global encryption settings for the archive
func (z *Zip) SetEncryption(e EncryptionMethod, pwd string) {
	z.encryptionMethod = e
	z.password = pwd
}

// AddFile adds a file from the filesystem to the ZIP archive.
// The provided [os.File] must be open and readable.
// The caller is responsible for closing the file after the ZIP archive is saved.
// Note: The file should not be modified between adding and saving the archive.
func (z *Zip) AddFile(f *os.File, options ...AddOption) error {
	if f == nil {
		return errors.New("file cannot be nil")
	}
	file, err := newFileFromOS(f)
	if err != nil {
		return fmt.Errorf("newFileFromOS: %v", err)
	}
	if file.isDir {
		return errors.New("AddFile: can't add directories")
	}

	file.config.CompressionMethod = z.compressionMethod
	for _, opt := range options {
		opt(file)
	}
	if file.config.Path != "" {
		z.ensurePath(file)
	}
	z.files = append(z.files, file)
	return nil
}

// AddReader adds a file to the ZIP archive from an [io.Reader] interface.
// This allows adding files from sources other than the filesystem, such as memory buffers or network streams.
// The filename parameter specifies the name that will be used for the file in the archive.
func (z *Zip) AddReader(r io.Reader, filename string, options ...AddOption) error {
	file, err := newFileFromReader(r, filename)
	if err != nil {
		return err
	}
	for _, opt := range options {
		opt(file)
	}
	if file.path != "" {
		z.ensurePath(file)
	}
	z.files = append(z.files, file)
	return nil
}

// CreateDirectory adds a directory entry to the ZIP archive
func (z *Zip) CreateDirectory(name string, options ...AddOption) error {
	file, err := newDirectoryFile("", name)
	if err != nil {
		return err
	}
	for _, opt := range options {
		opt(file)
	}
	if file.path != "" {
		err := z.ensurePath(file)
		if err != nil {
			return fmt.Errorf("ensure path: %v", err)
		}
	}
	z.files = append(z.files, file)
	return nil
}

// Exists checks if file or directory with given name exists at the specified path.
// Returns true if an entry with matching path and filename is found in the archive.
func (z *Zip) Exists(filepath, filename string) bool {
	for _, file := range z.files {
		if file.path == filepath && file.name == filename {
			return true
		}
	}
	return false
}

// Save writes the ZIP archive to disk with the specified filename.
// Returns error if any I/O operation fails during the save process.
func (z *Zip) Save(name string) error {
	dest, err := os.Create(name)
	if err != nil {
		return err
	}
	defer dest.Close()

	var tmpFile *os.File
	defer func() {
		if tmpFile != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
		}
	}()

	writer := newZipWriter(z, dest)
	for _, file := range z.files {
		meta := NewFileMetadata(file)
		meta.AddFilesystemExtraField()

		writer.WriteFileHeader(file)
		if !file.isDir {
			tmpFile, err = writer.EncodeFileData(file)
			if err != nil {
				return err
			}
		}
		if file.RequiresZip64() {
			meta.addZip64ExtraField()
		}
		err = writer.UpdateLocalHeader(file)
		if err != nil {
			return fmt.Errorf("update local header: %v", err)
		}
		writer.WriteFileData(tmpFile)
		writer.AddCentralDirEntry(file)
		if tmpFile != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
		}
	}
	writer.WriteCentralDirAndEndDir()
	return nil
}

// ensurePath verifies that the file's directory path exists in the archive,
// creating any missing parent directories if necessary.
func (z *Zip) ensurePath(f *file) error {
	if f.config.Path == "" || f.config.Path == "/" {
		return nil
	}

	normalizedPath := path.Clean(f.config.Path)
	if normalizedPath == "." || normalizedPath == "/" {
		return nil
	}
	f.path = normalizedPath
	pathComponents := strings.Split(normalizedPath, "/")

	currentPath := ""
	for _, component := range pathComponents {
		if component == "" {
			continue
		}

		if !z.Exists(currentPath, component) {
			dir, err := newDirectoryFile(currentPath, component)
			if err != nil {
				return fmt.Errorf("create directory file: %v", err)
			}
			z.files = append(z.files, dir)
		}

		if currentPath == "" {
			currentPath = component
		} else {
			currentPath = path.Join(currentPath, component)
		}
	}

	return nil
}
