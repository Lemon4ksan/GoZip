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

// Compressor defines a strategy for compressing data. See [DeflateCompressor] as an example
type Compressor interface {
	// Compress reads from src, compresses the data, and writes to dest.
	// Returns the number of uncompressed bytes read from src.
	Compress(src io.Reader, dest io.Writer) (int64, error)
}

// ZipConfig holds configuration options for archive
type ZipConfig struct {
	// CompressionMethod specifies default compression method
	CompressionMethod CompressionMethod

	// CompressionLevel specifies default compression level
	CompressionLevel int

	// EncryptionMethod specifies the encryption method
	EncryptionMethod  EncryptionMethod

	// FileSortStrategy specifies the strategy for file sorting before writing
	FileSortStrategy  FileSortStrategy

	// Comment stores archive comment
	Comment           string

	// Password stores password for encryption
	Password          string
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

	// Name specifies the file's name and path within the archive
	Name string
}

// AddOption defines a function type for configuring file options
type AddOption func(f *file)

// WithConfig applies a complete FileConfig to a file
func WithConfig(c FileConfig) AddOption {
	return func(f *file) {
		f.SetConfig(c)
	}
}

// WithCompression sets a compression for file
func WithCompression(c CompressionMethod, lvl int) AddOption {
	return func(f *file) {
		if !f.isDir {
			f.config.CompressionMethod = c
			f.config.CompressionLevel = lvl
		}
	}
}

// WithName sets file's name and path within the archive
func WithName(name string) AddOption {
	return func(f *file) {
		if name != "" {
			f.name = name
		}
	}
}

// WithPath prefixes file's path within the archive
func WithPath(p string) AddOption {
	return func(f *file) {
		if p != "" && p != "." {
			f.name = path.Join(p, f.name)
		}
	}
}

// Zip represents an editable ZIP archive in memory
type Zip struct {
	mu          sync.RWMutex
	compressors sync.Map

	config      ZipConfig
	files       []*file
	fileCache   map[string]bool
}

// NewZip creates a new empty ZIP archive
func NewZip() *Zip {
	return &Zip{
		files:     make([]*file, 0),
		fileCache: make(map[string]bool),
	}
}

// GetFiles returns files stored in the archive
func (z *Zip) GetFiles() []*file {
	return z.files
}

// SetConfig sets config for zip
func (z *Zip) SetConfig(c ZipConfig) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.config = c
}

// RegisterCompressor registers a custom compressor for a specific compression method
func (z *Zip) RegisterCompressor(method CompressionMethod, level int, c Compressor) {
	z.compressors.Store(fmt.Sprintf("%d::%d", method, level), c)
}

// AddFile adds an existing open file to the archive
func (z *Zip) AddFile(f *os.File, options ...AddOption) error {
	if f == nil {
		return errors.New("file cannot be nil")
	}
	fileEntry, err := newFileFromOS(f)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
}

// AddFromPath adds a file specified by fsPath.
// This is the preferred method as it manages file descriptors efficiently.
func (z *Zip) AddFromPath(fsPath string, options ...AddOption) error {
	fileEntry, err := newFileFromPath(fsPath)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
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

		pathOpt := WithPath(filepath.ToSlash(filepath.Dir(relPath)))
		fileOpts := append([]AddOption{pathOpt}, options...)
		if err := z.AddFromPath(walkPath, fileOpts...); err != nil {
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
	return z.addEntry(fileEntry, options)
}

// CreateDirectory adds a directory entry to the ZIP archive
func (z *Zip) CreateDirectory(name string, options ...AddOption) error {
	if name == "" {
		return errors.New("directory name cannot be empty")
	}

	dirEntry, err := newDirectoryFile(name)
	if err != nil {
		return fmt.Errorf("create directory file: %w", err)
	}
	return z.addEntry(dirEntry, options)
}

// Exists checks if file or directory with given name exists
func (z *Zip) Exists(name string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	key := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	return z.fileCache[key] || z.fileCache[key+"/"]
}

// Write writes the archive sequentially to dest
func (z *Zip) Write(dest io.WriteSeeker) error {
	z.mu.RLock()
	filesSnapshot := make([]*file, len(z.files))
	copy(filesSnapshot, z.files)
	z.mu.RUnlock()

	writer := newZipWriter(z, dest)
	sortedFiles := SortFilesOptimized(filesSnapshot, z.config.FileSortStrategy)
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
	sortedFiles := SortFilesOptimized(filesSnapshot, z.config.FileSortStrategy)
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

// addEntry verifies validity and adds file to the archive
func (z *Zip) addEntry(f *file, options []AddOption) error {
	if !f.isDir {
		f.config.CompressionMethod = z.config.CompressionMethod
		f.config.CompressionLevel = z.config.CompressionLevel
	}
	for _, opt := range options {
		opt(f)
	}
	f.name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(f.name, "\\", "/")), "/")

	z.mu.Lock()
	defer z.mu.Unlock()

	if z.fileCache[f.name] {
		return fmt.Errorf("duplicate entry: %s", f.name)
	}
	if !f.isDir && z.fileCache[f.name+"/"] {
		return fmt.Errorf("collision: %s is already a directory", f.name)
	}
	if f.isDir && z.fileCache[strings.TrimSuffix(f.name, "/")] {
		return fmt.Errorf("collision: directory %s conflicts with existing file", f.name)
	}

	if err := z.createParentDirs(f.name); err != nil {
		return fmt.Errorf("create parent dirs: %w", err)
	}

	z.files = append(z.files, f)
	if f.isDir {
		z.fileCache[f.name+"/"] = true
	} else {
		z.fileCache[f.name] = true
	}
	return nil
}

// createParentDirs creates missing directories according to path
func (z *Zip) createParentDirs(filePath string) error {
	dir := path.Dir(filePath)
	if dir == "." || dir == "/" {
		return nil
	}
	if z.fileCache[dir+"/"] {
		return nil
	}

	var missingDirs []string
	for dir != "." && dir != "/" {
		if z.fileCache[dir+"/"] {
			break
		}
		if z.fileCache[dir] {
			return fmt.Errorf("path collision: '%s' is a file, cannot create directory", dir)
		}
		missingDirs = append(missingDirs, dir)
		dir = path.Dir(dir)
	}

	for i := len(missingDirs) - 1; i >= 0; i-- {
		missingDir := missingDirs[i]

		dirEntry, err := newDirectoryFile(missingDir)
		if err != nil {
			return err
		}

		z.files = append(z.files, dirEntry)
		z.fileCache[missingDir+"/"] = true
	}

	return nil
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
