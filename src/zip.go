package gozip

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// CompressionMethod represents the compression algorithm used for a file in the ZIP archive
type CompressionMethod uint16

// Supported compression methods according to ZIP specification
const (
	Stored   CompressionMethod = 0 // No compression - file stored as-is
	Deflated CompressionMethod = 8 // DEFLATE compression (most common)
)

// Compression levels for DEFLATE algorithm
const (
	DeflateNormal    = 6 // Default compression level (good balance between speed and ratio)
	DeflateMaximum   = 9 // Maximum compression (best ratio, slowest speed)
	DeflateFast      = 3 // Fast compression (lower ratio, faster speed)
	DeflateSuperFast = 1 // Super fast compression (lowest ratio, fastest speed)
)

// EncryptionMethod represents the encryption algorithm used for file protection
type EncryptionMethod uint16

// Supported encryption methods
const (
	NotEncrypted EncryptionMethod = 0 // No encryption - file stored in plaintext
)

// AddOption defines a function type for configuring file options during addition to archive
type AddOption func(f *file)

// WithCompressionMethod sets a specific compression method for a file
func WithCompressionMethod(c CompressionMethod) AddOption {
	return func(f *file) {
		f.compressionMethod = c
	}
}

// WithCompressionLevel sets a specified algorithm dependent compression level
func WithCompressionLevel(l int) AddOption {
	return func(f *file) {
		f.compressionLevel = l
	}
}

// WithComment adds a comment to a specific file in the archive
func WithComment(cmt string) AddOption {
	return func(f *file) {
		f.comment = cmt
	}
}

// WithCustomModTime sets a specified time as file's last modification time.
// If not set, current system time is used for new files or file's actual mod time for existing files.
func WithCustomModTime(t time.Time) AddOption {
	return func(f *file) {
		f.modTime = t
	}
}

// WithPath sets path in which the file will be located in zip archive.
// Parent directories are automatically created if they don't exist.
func WithPath(path string) AddOption {
	return func(f *file) {
		f.path = path
	}
}

// ExtraFieldEntry represents an external file data.
type ExtraFieldEntry struct {
	Tag  uint16
	Data []byte
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
		return fmt.Errorf("create file from OS: %v", err)
	}
	if file.isDir {
		return errors.New("AddFile: can't add directories")
	}
	file.compressionMethod = z.compressionMethod

	for _, opt := range options {
		opt(file)
	}

	z.files = append(z.files, file)

	if file.path != "" {
		z.ensurePath(file)
	}

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

	z.files = append(z.files, file)

	if file.path != "" {
		z.ensurePath(file)
	}

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

	z.files = append(z.files, file)

	if file.path != "" {
		z.ensurePath(file)
	}

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
		file.updateFileMetadataExtraField()
		header := file.localHeader()
		headerOffset := localHeaderOffset
		file.localHeaderOffset = uint32(headerOffset)

		n, err := f.Write(header.encode(file))
		if err != nil {
			return fmt.Errorf("error writing local file header: %v", err)
		}
		localHeaderOffset += int64(n)

		// Write compressed file data
		if !file.isDir {
			err = file.compressAndWrite(f)
			if err != nil {
				return fmt.Errorf("error writing compressed file data: %v", err)
			}
			localHeaderOffset += file.compressedSize

			// Update local header with actual CRC and compressed size
			err = file.updateLocalHeader(f, headerOffset)
			if err != nil {
				return fmt.Errorf("error updating local file header: %v", err)
			}
		}

		cdData := file.centralDirEntry()
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

	// Write end of central directory record
	endOfCentralDir := encodeEndOfCentralDirRecord(
		z,
		uint32(sizeOfCentralDir),
		uint32(localHeaderOffset),
	)
	_, err = f.Write(endOfCentralDir)
	if err != nil {
		return fmt.Errorf("error writing end of central directory to zip archive: %v", err)
	}

	return nil
}

// ensurePath verifies that the file's directory path exists in the archive,
// creating any missing parent directories if necessary.
func (z *Zip) ensurePath(f *file) error {
	if f.path == "" || f.path == "/" {
		return nil
	}

	normalizedPath := path.Clean(f.path)
	if normalizedPath == "." || normalizedPath == "/" {
		return nil
	}
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