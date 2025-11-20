package gozip

import (
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Compressor defines a strategy for compressing data.
// Implementations are responsible for thread-safety and memory pooling.
// See [DeflateCompressor] as an example on how to implement efficient compressor.
type Compressor interface {
	// Compress reads from src, compresses the data, and writes to dest.
	// Returns the number of uncompressed bytes read from src.
	Compress(src io.Reader, dest io.Writer) (int64, error)
}

// FileConfig holds configuration options for individual files in the archive
type FileConfig struct {
	// CompressionMethod specifies the standard compression method to use
	CompressionMethod CompressionMethod

	// CompressionLevel controls the compression strength (0-9).
	// Higher values typically provide better compression at the cost of CPU time.
	CompressionLevel int

	// Comment stores optional comment string
	Comment string

	// IsEncrypted indicates whether the file should be encrypted
	IsEncrypted bool

	// Path specifies the file's location and name within the archive
	Path string
}

// AddOption defines a function type for configuring file options
type AddOption func(f *file)

// WithConfig applies a complete FileConfig to a file
func WithConfig(c FileConfig) AddOption {
	return func(f *file) {
		f.SetConfig(c)
	}
}

// WithCompression sets a compression method for file
func WithCompressionMethod(c CompressionMethod) AddOption {
	return func(f *file) {
		f.config.CompressionMethod = c
	}
}

// WithCompression sets a compression level for file
func WithCompressionLevel(lvl int) AddOption {
	return func(f *file) {
		f.config.CompressionLevel = lvl
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

// Zip represents an editable ZIP archive in memory
type Zip struct {
	compressionMethod CompressionMethod
	encryptionMethod  EncryptionMethod
	fileSortStrategy  FileSortStrategy
	files             []*file
	comment           string
	password          string

	// Concurrency safety
	mu       sync.RWMutex
	dirCache map[string]bool

	// Registry for reusable compressors
	compressors sync.Map
}

// NewZip creates a new empty ZIP archive
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

// RegisterCompressor registers a custom compressor for a specific compression method.
// The compressor instance must be thread-safe (e.g. use internal sync.Pool).
// This allows overriding standard methods or adding support for new ones (e.g. Zstd, Brotli).
func (z *Zip) RegisterCompressor(method CompressionMethod, level int, c Compressor) {
	z.compressors.Store(fmt.Sprintf("%d::%d", method, level), c)
}

// SetComment sets a global comment for the archive
func (z *Zip) SetComment(comment string) {
	z.mu.Lock()
	defer z.mu.Unlock()
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

// AddFile adds an existing open file to the archive.
// NOTE: For best performance and resource management, use [Zip.AddFromPath] instead.
// This method reads metadata immediately but defers reading content until Write.
func (z *Zip) AddFile(f *os.File, options ...AddOption) error {
	if f == nil {
		return errors.New("file cannot be nil")
	}
	fileEntry, err := newFileFromOS(f)
	if err != nil {
		return err
	}

	if !fileEntry.isDir {
		fileEntry.config.CompressionMethod = z.compressionMethod
	}

	for _, opt := range options {
		opt(fileEntry)
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if err := z.ensurePath(fileEntry); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, fileEntry)
	return nil
}

// AddFromPath adds a file specified by fsPath.
// This is the preferred method as it manages file descriptors efficiently.
func (z *Zip) AddFromPath(fsPath string, options ...AddOption) error {
	fileEntry, err := newFileFromPath(fsPath)
	if err != nil {
		return err
	}

	if !fileEntry.isDir {
		fileEntry.config.CompressionMethod = z.compressionMethod
	}

	for _, opt := range options {
		opt(fileEntry)
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if err := z.ensurePath(fileEntry); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, fileEntry)
	return nil
}

// AddFromDir recursively adds files from a directory
func (z *Zip) AddFromDir(root string, options ...AddOption) error {
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

		localOpts := append(options, ExtendPath(filepath.ToSlash(filepath.Dir(relPath))))

		if err := z.AddFromPath(walkPath, localOpts...); err != nil {
			return fmt.Errorf("add file %s: %w", walkPath, err)
		}
		return nil
	})
}

// AddReader adds a file from an io.Reader
func (z *Zip) AddReader(r io.Reader, filename string, options ...AddOption) error {
	if r == nil {
		return errors.New("reader cannot be nil")
	}
	if filename == "" {
		return errors.New("filename cannot be empty")
	}

	fileEntry, _ := newFileFromReader(r, filename)
	fileEntry.config.CompressionMethod = z.compressionMethod

	for _, opt := range options {
		opt(fileEntry)
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if err := z.ensurePath(fileEntry); err != nil {
		return fmt.Errorf("ensure path: %w", err)
	}

	z.files = append(z.files, fileEntry)
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
	return z.existsInCache(filepath, filename)
}

// Write writes the archive sequentially to dest
func (z *Zip) Write(dest io.WriteSeeker) error {
	z.mu.RLock()
	filesSnapshot := make([]*file, len(z.files))
	copy(filesSnapshot, z.files)
	z.mu.RUnlock()

	writer := newZipWriter(z, dest)
	sortedFiles := SortFilesOptimized(filesSnapshot, z.fileSortStrategy)
	for _, file := range sortedFiles {
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("write file %s: %w", file.name, err)
		}
	}
	return writer.WriteCentralDirAndEndRecords()
}

// WriteParallel writes the archive using multiple workers
func (z *Zip) WriteParallel(dest io.WriteSeeker, maxWorkers int) error {
	z.mu.RLock()
	filesSnapshot := make([]*file, len(z.files))
	copy(filesSnapshot, z.files)
	z.mu.RUnlock()

	writer := newParallelZipWriter(z, dest, maxWorkers)
	sortedFiles := SortFilesOptimized(filesSnapshot, z.fileSortStrategy)
	errs := writer.WriteFiles(sortedFiles)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Internal helpers

// ensurePath verifies and creates directory structure (Unsafe: requires external Lock)
func (z *Zip) ensurePath(f *file) error {
	if f.config.Path == "" || f.config.Path == "/" {
		return nil
	}

	normalizedPath := path.Clean(strings.ReplaceAll(f.config.Path, "\\", "/"))
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

		if !z.existsInCache(currentPath, component) {
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

func (z *Zip) cacheDirectoryEntry(dir *file) {
	if dir == nil || !dir.isDir {
		return
	}
	cacheKey := z.buildCacheKey(dir.path, dir.name)
	z.dirCache[cacheKey] = true
}

func (z *Zip) existsInCache(path, name string) bool {
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

func (z *Zip) buildCacheKey(p, name string) string {
	if p == "" {
		return name + "/"
	}
	return path.Join(p, name) + "/"
}

// resolveCompressor determines the correct compressor for a file.
func (z *Zip) resolveCompressor(file *file) (Compressor, error) {
	if file.config.CompressionMethod == Stored {
		return &StoredCompressor{}, nil
	}

	key := fmt.Sprintf("%d::%d", file.config.CompressionMethod, file.config.CompressionLevel)
	if val, ok := z.compressors.Load(key); ok {
		return val.(Compressor), nil
	}

	if file.config.CompressionMethod == Deflated {
		level := file.config.CompressionLevel
		if level == 0 {
			level = flate.DefaultCompression
		}

		key := fmt.Sprintf("__deflate::%d", level)
		if val, ok := z.compressors.Load(key); ok {
			return val.(Compressor), nil
		}

		comp := NewDeflateCompressor(level)
		actual, _ := z.compressors.LoadOrStore(key, comp)
		return actual.(Compressor), nil
	}

	return nil, fmt.Errorf("unsupported compression method: %d", file.config.CompressionMethod)
}

// StoredCompressor implements no compression (STORE method)
type StoredCompressor struct{}

func (sc *StoredCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	return io.Copy(dest, src)
}

// DeflateCompressor implements DEFLATE compression with memory pooling
type DeflateCompressor struct {
	pool sync.Pool
}

// NewDeflateCompressor creates a reusable compressor for a specific level
func NewDeflateCompressor(level int) *DeflateCompressor {
	return &DeflateCompressor{
		pool: sync.Pool{
			New: func() interface{} {
				w, _ := flate.NewWriter(io.Discard, level)
				return w
			},
		},
	}
}

func (d *DeflateCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	w := d.pool.Get().(*flate.Writer)
	defer d.pool.Put(w)

	w.Reset(dest)

	n, err := io.Copy(w, src)
	if err != nil {
		return n, err
	}

	if err := w.Close(); err != nil {
		return n, err
	}

	return n, nil
}
