package gozip

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
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
		return fmt.Errorf("create directory file: %v", err)
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
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()

	var localHeaderOffset, sizeOfCentralDir int64
	centralDirBuf := new(bytes.Buffer)

	for _, file := range z.files {
		h, meta := newZipHeaders(file), NewFileMetadata(file)
		meta.SetupFileMetadataExtraField()
		header := h.LocalHeader()
		headerOffset := localHeaderOffset
		file.localHeaderOffset = headerOffset

		n, err := f.Write(header.encode(file))
		if err != nil {
			return fmt.Errorf("error writing local file header: %v", err)
		}
		localHeaderOffset += int64(n)

		// Write compressed file data
		if !file.isDir {
			op := NewFileOperations(file)
			err = op.CompressAndWrite(f)
			if err != nil {
				return fmt.Errorf("error writing compressed file data: %v", err)
			}
			localHeaderOffset += file.compressedSize

			// Update local header with actual CRC and compressed size
			err = op.UpdateLocalHeader(f, headerOffset)
			if err != nil {
				return fmt.Errorf("error updating local file header: %v", err)
			}
		}

		if file.RequiresZip64() {
			metadata := NewFileMetadata(file)
			metadata.addZip64ExtraField()
		}
		cdData := h.CentralDirEntry()
		n, err = centralDirBuf.Write(cdData.encode(file))
		if err != nil {
			return fmt.Errorf("error writing central directory entry to buffer: %v", err)
		}
		sizeOfCentralDir += int64(n)
	}

	// Write central directory to archive
	centralDirData := centralDirBuf.Bytes()
	_, err = f.Write(centralDirData)
	if err != nil {
		return fmt.Errorf("error writing central directory to zip archive: %v", err)
	}

	if sizeOfCentralDir > math.MaxUint32 || localHeaderOffset > math.MaxUint32 {
		zip64EndOfCentralDir := encodeZip64EndOfCentralDirectoryRecord(z, uint64(sizeOfCentralDir), uint64(localHeaderOffset))
		_, err := f.Write(zip64EndOfCentralDir)
		if err != nil {
			return fmt.Errorf("error writing zip64 end of central directory to zip archive: %v", err)
		}
		zip64EndOfCentralDirLocator := encodeZip64EndOfCentralDirectoryLocator(uint64(localHeaderOffset + sizeOfCentralDir))
		_, err = f.Write(zip64EndOfCentralDirLocator)
		if err != nil {
			return fmt.Errorf("error writing zip64 end of central directory locator to zip archive: %v", err)
		}
	}
	// Write end of central directory record
	endOfCentralDir := encodeEndOfCentralDirRecord(z, sizeOfCentralDir, localHeaderOffset)
	_, err = f.Write(endOfCentralDir)
	if err != nil {
		return fmt.Errorf("error writing end of central directory to zip archive: %v", err)
	}

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
