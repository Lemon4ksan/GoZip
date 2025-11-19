package gozip

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// CompressFunc is a function that compresses data from src, writes it to dest and returns
// the amount of bytes read (uncompressed size). See [WriteDeflated] as an example.
type CompressFunc func(src io.Reader, dest io.Writer, level int) (int64, error)

// FileConfig holds configuration options for individual files in the archive
type FileConfig struct {
	// CompressFunc defines the compression algorithm to use for this file.
	// If nil, uses the default compressor based on CompressionMethod.
	CompressFunc CompressFunc

	// CompressionMethod specifies the standard compression method to use.
	// It should match the CompressFunc algorithm implementation.
	CompressionMethod CompressionMethod

	// CompressionLevel controls the compression strength (1-9).
	// Higher values typically provide better compression at the cost of CPU time.
	// The exact meaning depends on the chosen compressor.
	// It's preferred that 0 means normal compression level.
	CompressionLevel int

	// Comment stores optional comment string
	Comment string

	// IsEncrypted indicates whether the file should be encrypted in the archive
	IsEncrypted bool

	// Path specifies the file's location and name within the archive
	Path string
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
// Provides methods to add files, configure compression/encryption, and write the archive.
type Zip struct {
	mu                sync.RWMutex
	compressionMethod CompressionMethod // Default compression for all files
	encryptionMethod  EncryptionMethod  // Encryption method for the archive
	password          string            // Password for encrypted archives
	files             []*file           // List of files in the archive
	comment           string            // Global archive comment
	dirCache          map[string]bool   // Cache of existing directory paths for faster lookup
	fileSortStrategy  FileSortStrategy  // In which order files must be processed
}

// NewZip creates a new empty ZIP archive with the specified default compression method.
// The compression method can be overridden per-file using [AddOption].
func NewZip(c CompressionMethod) *Zip {
	return &Zip{
		compressionMethod: c,
		files:             make([]*file, 0),
		dirCache:          make(map[string]bool),
	}
}

// SetFileSortStrategy configures file ordering to optimize archive creation.
// See [FileSortStrategy] for detailed description of strategies.
func (z *Zip) SetFileSortStrategy(strategy FileSortStrategy) {
	z.fileSortStrategy = strategy
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

// GetFiles returns files stored in the archive
func (z *Zip) GetFiles() []*file {
	return z.files
}

// AddFile adds a file from the filesystem at given path to the ZIP archive
func (z *Zip) AddFile(filePath string, options ...AddOption) error {
	file, err := newFileFromPath(filePath)
	if err != nil {
		return fmt.Errorf("newFileFromPath: %w", err)
	}
	if !file.isDir {
		file.config.CompressionMethod = z.compressionMethod
	}

	for _, opt := range options {
		opt(file)
	}

	if file.config.CompressFunc == nil {
		if err = file.SetDefaultCompressFunc(); err != nil {
			return err
		}
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if err := z.ensurePath(file); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, file)
	return nil
}

// AddDirectory recursively goes through the directory at given path
// and adds all files excluding root to the archive, applying options to each of them.
func (z *Zip) AddDirectory(root string, options ...AddOption) error {
	return filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, err error) error {
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

		addOptions := append(options, ExtendPath(filepath.Dir(relPath)))
		return z.AddFile(walkPath, addOptions...)
	})
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
	file.config.CompressionMethod = z.compressionMethod

	for _, opt := range options {
		opt(file)
	}

	if file.config.CompressFunc == nil {
		if err := file.SetDefaultCompressFunc(); err != nil {
			return err
		}
	}

	z.mu.Lock()
	defer z.mu.Unlock()

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

	z.mu.Lock()
	defer z.mu.Unlock()

	if err := z.ensurePath(file); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, file)
	z.cacheDirectoryEntry(file)
	return nil
}

// Exists checks if file or directory with given name exists at the specified path.
// Returns true if an entry with matching path and filename is found in the archive.
func (z *Zip) Exists(filepath, filename string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	if filename == "" || strings.HasSuffix(filename, "/") {
		cacheKey := z.buildCacheKey(filepath, filename)
		if _, exists := z.dirCache[cacheKey]; exists {
			return true
		}
	}

	for _, file := range z.files {
		if file.path == filepath && file.name == filename {
			return true
		}
	}
	return false
}

// Write writes the ZIP archive to dest.
// Returns error if any I/O operation fails during the save process.
func (z *Zip) Write(dest io.WriteSeeker) error {
	z.mu.RLock() 
	filesToWrite := make([]*file, len(z.files))
    copy(filesToWrite, z.files)
	z.mu.RUnlock()

	writer := newZipWriter(z, dest)
	sortedFiles := SortFilesOptimized(filesToWrite, z.fileSortStrategy)
	for _, file := range sortedFiles {
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("write file %s: %w", file.name, err)
		}
	}
	return writer.WriteCentralDirAndEndRecords()
}

// WriteParallel writes the ZIP archive to dest using multiple workers for parallel compression
func (z *Zip) WriteParallel(dest io.WriteSeeker, maxWorkers int) error {
	z.mu.RLock() 
	filesToWrite := make([]*file, len(z.files))
    copy(filesToWrite, z.files)
    z.mu.RUnlock()

	writer := newParallelZipWriter(z, dest, maxWorkers)
	sortedFiles := SortFilesOptimized(filesToWrite, z.fileSortStrategy)
	errs := writer.WriteFiles(sortedFiles)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
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

		if !z.directoryExistsInCache(currentPath, component) {
			dir, err := newDirectoryFile(currentPath, component)
			if err != nil {
				return fmt.Errorf("create directory file: %w", err)
			}
			z.files = append(z.files, dir)
			z.cacheDirectoryEntry(dir)
		}

		if currentPath == "" {
			currentPath = component
		} else {
			currentPath = path.Join(currentPath, component)
		}
	}

	return nil
}

// cacheDirectoryEntry adds a directory file to the directory cache for faster lookups.
// This method should be called whenever a new directory is added to the archive.
func (z *Zip) cacheDirectoryEntry(dir *file) {
	if dir == nil || !dir.isDir {
		return
	}
	cacheKey := z.buildCacheKey(dir.path, dir.name)
	z.dirCache[cacheKey] = true
}

// directoryExistsInCache checks if a directory exists using the cache with fallback.
// Returns true if the directory is found in cache or through linear scan.
func (z *Zip) directoryExistsInCache(path, name string) bool {
	cacheKey := z.buildCacheKey(path, name)
	if _, exists := z.dirCache[cacheKey]; exists {
		return true
	}

	for _, file := range z.files {
		if file.path == path && file.name == name {
            z.dirCache[cacheKey] = true
			return true
		}
	}

	return false
}

// buildCacheKey creates a unique cache key for a directory entry
func (z *Zip) buildCacheKey(p, name string) string {
	if p == "" {
		return name + "/"
	}
	return path.Join(p, name) + "/"
}

// ClearCache resets the directory cache
func (z *Zip) ClearCache() {
	z.dirCache = make(map[string]bool)
}