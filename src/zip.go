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
	"sync"
	"time"
)

// ZipConfig holds configuration options for archive
type ZipConfig struct {
	// CompressionMethod specifies default compression method
	CompressionMethod CompressionMethod

	// CompressionLevel specifies default compression level
	CompressionLevel int

	// EncryptionMethod specifies the encryption method
	EncryptionMethod EncryptionMethod

	// FileSortStrategy specifies the strategy for file sorting before writing
	FileSortStrategy FileSortStrategy

	// Comment stores archive comment
	Comment string

	// Password stores password for encryption
	Password string
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

// WithMode sets file mode bits
func WithMode(mode fs.FileMode) AddOption {
	return func(f *file) {
		f.mode = mode
	}
}

// Zip represents an editable ZIP archive in memory
type Zip struct {
	mu            sync.RWMutex
	config        ZipConfig
	files         []*file
	fileCache     map[string]bool
	compressors   map[string]Compressor
	decompressors map[CompressionMethod]Decompressor
	bufferPool    sync.Pool
}

// NewZip creates a new empty ZIP archive object.
// Note that only [Stored] and [Deflated] compression methods are supported by default.
// You can add your implementation by using AddCompressor and AddDecompressor.
func NewZip() *Zip {
	return &Zip{
		files:         make([]*file, 0),
		fileCache:     make(map[string]bool),
		compressors:   make(map[string]Compressor),
		decompressors: make(map[CompressionMethod]Decompressor),
		bufferPool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 64*1024) // 64KB buffer
				return &b
			},
		},
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
	z.mu.Lock()
	defer z.mu.Unlock()
	z.compressors[fmt.Sprintf("%d::%d", method, level)] = c
}

// RegisterDecompressor registers a custom decompressor for a specific compression method
func (z *Zip) RegisterDecompressor(method CompressionMethod, c Decompressor) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.decompressors[method] = c
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
func (z *Zip) AddFromPath(path string, options ...AddOption) error {
	fileEntry, err := newFileFromPath(path)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
}

// AddFromDir recursively adds files from a directory
func (z *Zip) AddFromDir(path string, options ...AddOption) error {
	return filepath.WalkDir(path, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if walkPath == path {
			return nil
		}

		relPath, err := filepath.Rel(path, walkPath)
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

// Write writes the archive sequentially to dest.
// It's more memory efficient compared to parallel version.
// If dest implements [io.Seeker], file source will be written
// to dest without creating temp file when possible.
func (z *Zip) Write(dest io.Writer) error {
	z.mu.RLock()
	filesSnapshot := make([]*file, len(z.files))
	copy(filesSnapshot, z.files)
	z.mu.RUnlock()

	writer := newZipWriter(z.config, z.compressors, dest)
	sortedFiles := SortFilesOptimized(filesSnapshot, z.config.FileSortStrategy)
	for _, file := range sortedFiles {
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("write file %s: %w", file.name, err)
		}
	}
	return writer.WriteCentralDirAndEndRecords()
}

// WriteParallel writes the archive using multiple workers.
// Temp files and memory buffers are used to store compressed data before writing.
func (z *Zip) WriteParallel(dest io.Writer, maxWorkers int) error {
	z.mu.RLock()
	filesSnapshot := make([]*file, len(z.files))
	copy(filesSnapshot, z.files)
	z.mu.RUnlock()

	writer := newParallelZipWriter(z.config, z.compressors, dest, maxWorkers)
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

// Read reads the archive and appends its files to struct files
func (z *Zip) Read(src io.ReadSeeker) error {
	reader := newZipReader(src, z.decompressors)
	files, err := reader.ReadFiles()
	z.files = append(z.files, files...)
	return err
}

// Extract sequentially extracts all files stored in archive to the disk at the given path
func (z *Zip) Extract(path string) error {
	// Alphabetical sort guarantees the right order of directories
	files := sortAlphabetical(z.files)
	path = filepath.Clean(path)
	var errs []error

	for _, f := range files {
		fpath := filepath.Join(path, f.name)

		if !strings.HasPrefix(fpath, path+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("illegal file path: %s", fpath))
			continue
		}
		if f.isDir {
			if err := os.Mkdir(fpath, 0755); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		if err := z.extractFile(f, fpath); err != nil {
			errs = append(errs, fmt.Errorf("failed to extract %s: %w", f.name, err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Extract parallel extracts all files stored in archive using multiple workers
func (z *Zip) ExtractParallel(path string, workers int) error {
	path = filepath.Clean(path)

	var filesToExtract []*file
	dirsToCreate := make(map[string]struct{})
	dirsToCreate[path] = struct{}{}

	var errs []error

	for _, f := range z.files {
		fpath := filepath.Join(path, f.name)

		if !strings.HasPrefix(fpath, path+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("illegal file path: %s", fpath))
			continue
		}

		if f.isDir {
			dirsToCreate[fpath] = struct{}{}
			continue
		}

		dirsToCreate[filepath.Dir(fpath)] = struct{}{}
		filesToExtract = append(filesToExtract, f)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	for dir := range dirsToCreate {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	sem := make(chan struct{}, workers)
	errChan := make(chan error, len(filesToExtract))
	var wg sync.WaitGroup

	for _, f := range filesToExtract {
		wg.Add(1)
		sem <- struct{}{}

		go func(f *file) {
			defer wg.Done()
			defer func() { <-sem }()

			fpath := filepath.Join(path, f.name)
			if err := z.extractFile(f, fpath); err != nil {
				errChan <- fmt.Errorf("failed to extract %s: %w", f.name, err)
			}
		}(f)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
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

	if err := z.createMissingDirs(f.name); err != nil {
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

// createMissingDirs creates missing directories according to path
func (z *Zip) createMissingDirs(filePath string) error {
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

// extractFile extracts single file to disk
func (z *Zip) extractFile(f *file, path string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(path)
	if err != nil {
		return err
	}

	if f.uncompressedSize > 0 {
		if err := dst.Truncate(f.uncompressedSize); err != nil {
			dst.Close()
			return err
		}
	}

	bufPtr := z.bufferPool.Get().(*[]byte)
	_, err = io.CopyBuffer(dst, src, *bufPtr)
	z.bufferPool.Put(bufPtr)

	if err != nil {
		dst.Close()
		return err
	}

	if err := dst.Close(); err != nil {
		return err
	}

	perm := f.mode & fs.ModePerm
	if perm == 0 {
		perm = 0644
	}
	os.Chmod(path, perm)
	os.Chtimes(path, time.Now(), f.modTime)

	return nil
}
