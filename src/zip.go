package gozip

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// WithConfig applies a complete FileConfig to a file
func WithConfig(c FileConfig) AddOption {
	return func(f *file) {
		f.SetConfig(c)
	}
}

// PrefixPath prefixes the file path with the given path component
func PrefixPath(p string) AddOption {
	return func(f *file) {
		if p != "" {
			f.config.Path = path.Join(p, f.config.Path)
		}
	}
}

// ExtendPath extends the file path with given path component
func ExtendPath(p string) AddOption {
	return func(f *file) {
		if p != "" {
			f.config.Path = path.Join(f.config.Path, p)
		}
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
		files:             make([]*file, 0),
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
		return fmt.Errorf("newFileFromOS: %w", err)
	}
	if file.isDir {
		return errors.New("AddFile: can't add directories, use AddDirectory instead")
	}

	file.config.CompressionMethod = z.compressionMethod
	for _, opt := range options {
		opt(file)
	}

	if err := z.ensurePath(file); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, file)
	return nil
}

// AddDirectory recursively goes through the directory at given path
// and adds all files excluding root to the archive, applying options to each of them.
// Returns list of opened files that must be closed after archive is saved or error occurs.
func (z *Zip) AddDirectory(root string, options ...AddOption) ([]*os.File, error) {
	files := make([]*os.File, 0)
	root = filepath.Clean(root)

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if walkPath == root {
			return nil
		}

		relPath, err := filepath.Rel(root, walkPath)
		if err != nil {
			return fmt.Errorf("get relative path: %w", err)
		}

		if d.IsDir() {
			file, err := newDirectoryFileFromDirEntry(d)
			if err != nil {
				return fmt.Errorf("create directory file: %w", err)
			}

			for _, opt := range append(options, ExtendPath(filepath.Dir(relPath))) {
				opt(file)
			}

			if err := z.ensurePath(file); err != nil {
				return fmt.Errorf("ensure path: %w", err)
			}
			z.files = append(z.files, file)
		} else {
			file, err := os.Open(walkPath)
			if err != nil {
				return fmt.Errorf("open file: %w", err)
			}

			if err := z.AddFile(file, append(options, ExtendPath(filepath.Dir(relPath)))...); err != nil {
				file.Close()
				return fmt.Errorf("add file: %w", err)
			}
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		for _, f := range files {
			f.Close()
		}
		return nil, err
	}
	return files, nil
}

// AddReader adds a file to the ZIP archive from an [io.Reader] interface.
// This allows adding files from sources other than the filesystem, such as memory buffers or network streams.
// The filename parameter specifies the name that will be used for the file in the archive.
func (z *Zip) AddReader(r io.Reader, filename string, options ...AddOption) error {
	if r == nil {
		return errors.New("reader cannot be nil")
	}
	if filename == "" {
		return errors.New("filename cannot be empty")
	}

	file, err := newFileFromReader(r, filename)
	if err != nil {
		return fmt.Errorf("create file from reader: %w", err)
	}

	for _, opt := range options {
		opt(file)
	}

	if err := z.ensurePath(file); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, file)
	return nil
}

// CreateDirectory adds a directory entry to the ZIP archive
func (z *Zip) CreateDirectory(name string, options ...AddOption) error {
	if name == "" {
		return errors.New("directory name cannot be empty")
	}

	file, err := newDirectoryFile("", name)
	if err != nil {
		return fmt.Errorf("create directory file: %w", err)
	}

	for _, opt := range options {
		opt(file)
	}

	if err := z.ensurePath(file); err != nil {
		return fmt.Errorf("ensure path: %w", err)
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

// Save writes the ZIP archive to dest.
// Returns error if any I/O operation fails during the save process.
func (z *Zip) Save(dest io.WriteSeeker) error {
	writer := newZipWriter(z, dest)
	for _, file := range z.files {
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("write file %s: %w", file.name, err)
		}
	}
	return writer.WriteCentralDirAndEndRecords()
}

// SaveParallel writes the ZIP archive to dest using multiple workers for parallel compression.
// Parallel compression is only effective for compression methods other than Stored.
// Returns a slice of errors encountered during compression.
func (z *Zip) SaveParallel(dest io.WriteSeeker, maxWorkers int) []error {
	writer := newParallelZipWriter(z, dest, maxWorkers)
	errs := writer.WriteFiles(z.files)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// ensurePath verifies that the file's directory path exists in the archive,
// creating any missing parent directories if necessary.
func (z *Zip) ensurePath(f *file) error {
	if f.config.Path == "" || f.config.Path == "/" {
		return nil
	}

	normalizedPath := filepath.ToSlash(filepath.Clean(f.config.Path))
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
				return fmt.Errorf("create directory file: %w", err)
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
