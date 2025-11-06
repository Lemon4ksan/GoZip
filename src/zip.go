package gozip

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
)

// CompressionMethod represents the compression algorithm used for a file in the ZIP archive
type CompressionMethod uint16

// Supported compression methods according to ZIP specification
const (
	Stored   CompressionMethod = 0 // No compression - file stored as-is
	Deflated CompressionMethod = 8 // DEFLATE compression (most common)
)

const (
	DeflateNormal = 6
	DeflateMaximum = 9
	DeflateFast = 3
	DeflateSuperFast = 1
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

// WithComment adds a comment to a specific file in the archive.
func WithComment(cmt string) AddOption {
	return func(f *file) {
		f.comment = cmt
	}
}

// WithCustomModTime sets a specified time as file's last modification time.
func WithCustomModTime(t time.Time) AddOption {
	return func(f *file) {
		f.modTime = t
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

// AddComment sets a global comment for the entire ZIP archive
func (z *Zip) AddComment(comment string) {
	z.comment = comment
}

// SetEncryption configures global encryption settings for the archive
func (z *Zip) SetEncryption(e EncryptionMethod, pwd string) {
	z.encryptionMethod = e
	z.password = pwd
}

// AddFile adds a file from the filesystem to the ZIP archive.
// The user is responsible for closing the added files after finishing working with the archive.
func (z *Zip) AddFile(f *os.File, options ...AddOption) error {
	file, err := newFileFromOS(f)
	if err != nil {
		return err
	}
	file.compressionMethod = z.compressionMethod

	for _, opt := range options {
		opt(file)
	}

	z.files = append(z.files, file)
	return nil
}

func (z *Zip) AddReader(r io.Reader, filename string, options ...AddOption) error {
	file, err := newFileFromReader(r, filename)
	if err != nil {
		return err
	}

	for _, opt := range options {
		opt(file)
	}

	z.files = append(z.files, file)
	return nil
}

// Save writes the ZIP archive to disk with the specified filename.
func (z *Zip) Save(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()

	var localHeaderOffset, sizeOfCentralDir int64
	centralDirBuf := new(bytes.Buffer)

	for _, file := range z.files {
		header := file.localHeader()
		headerOffset := localHeaderOffset

		n, err := f.Write(header.encode(file))
		if err != nil {
			return fmt.Errorf("error writing local file header: %v", err)
		}
		localHeaderOffset += int64(n)

		// Write compressed file data
		err = file.compressAndWrite(f)
		if err != nil {
			return fmt.Errorf("error writing compressed file data: %v", err)
		}
		localHeaderOffset += file.compressedSize

		// Update local header with actual CRC and compressed size
		file.localHeaderOffset = uint32(headerOffset)
		err = file.updateLocalHeader(f, headerOffset)
		if err != nil {
			return fmt.Errorf("error updating local file header: %v", err)
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
