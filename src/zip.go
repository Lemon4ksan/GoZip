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

// ZipConfig defines global configuration parameters for creating or modifying ZIP archives.
// These settings apply to the entire archive unless overridden at the file level.
type ZipConfig struct {
	// CompressionMethod specifies the default compression algorithm used for files
	CompressionMethod CompressionMethod

	// CompressionLevel controls the trade-off between compression ratio and speed.
	// Range is typically 0-9, where 0 means no compression (fastest) and 9 means
	// maximum compression (slowest). Specific meaning depends on the CompressionMethod.
	CompressionLevel int

	// Password provides the default encryption password for the entire archive.
	// This password will be used for all encrypted files unless overridden per file.
	Password string

	// EncryptionMethod selects the encryption algorithm for the archive.
	// Options include NotEncrypted, ZipCrypto (legacy), and AES256 (recommended).
	EncryptionMethod EncryptionMethod

	// FileSortStrategy determines how files are ordered when writing the archive.
	// Different strategies can optimize for read performance, streaming, or
	// directory traversal efficiency.
	FileSortStrategy FileSortStrategy

	// Comment stores an optional text comment for the entire ZIP archive.
	// Maximum length is 65535 bytes due to ZIP format limitations.
	Comment string
}

// FileConfig defines per-file configuration options that override archive defaults.
// These settings allow fine-grained control over individual archive entries.
type FileConfig struct {
	// Name specifies the file's path and filename within the ZIP archive.
	// Directories shouldn't necessarily end with slashes, as they'll be added automatically.
	Name string

	// Password provides an encryption password specific to this file,
	// overriding the archive-level password if set.
	Password string

	// CompressionMethod selects the compression algorithm for this specific file,
	// overriding the archive default.
	CompressionMethod CompressionMethod

	// EncryptionMethod selects the encryption algorithm for this specific file,
	// overriding the archive default.
	EncryptionMethod EncryptionMethod

	// CompressionLevel controls compression strength for this file (0-9),
	// overriding the archive default. Higher values increase compression ratio
	// at the cost of CPU time and memory.
	CompressionLevel int

	// Comment stores an optional text comment attached to this specific file.
	// Maximum length is 65535 bytes due to ZIP format limitations.
	Comment string
}

// AddOption represents a functional option pattern for configuring File objects.
// This pattern provides a clean, extensible API for setting file properties.
type AddOption func(f *File)

// WithConfig applies a complete FileConfig to a File, overriding all configurable properties.
// This is useful when you have a pre-configured FileConfig object or need to apply
// multiple settings atomically.
func WithConfig(c FileConfig) AddOption {
	return func(f *File) {
		f.SetConfig(c)
	}
}

// WithCompression configures the compression method and level for a specific file.
// This option only affects regular files (not directories) and overrides both
// archive defaults and any previously set compression settings.
func WithCompression(c CompressionMethod, lvl int) AddOption {
	return func(f *File) {
		if !f.isDir {
			f.config.CompressionMethod = c
			f.config.CompressionLevel = lvl
		}
	}
}

// WithEncryption configures encryption settings for a specific file.
// This option only affects regular files (not directories) and overrides both
// archive defaults and any previously set encryption settings.
func WithEncryption(e EncryptionMethod, pwd string) AddOption {
	return func(f *File) {
		if !f.isDir {
			f.config.EncryptionMethod = e
			f.config.Password = pwd
		}
	}
}

// WithName sets or changes the filename and path within the ZIP archive.
// The name is normalized to use forward slashes and cleaned to prevent directory
// traversal issues. An empty name is ignored.
func WithName(name string) AddOption {
	return func(f *File) {
		if name != "" {
			f.name = name
		}
	}
}

// WithPath prefixes the file's current name with the specified directory path.
// This is useful for organizing files into subdirectories within the archive.
// The path "." is treated as no path (current directory).
func WithPath(p string) AddOption {
	return func(f *File) {
		if p != "" && p != "." {
			f.name = path.Join(p, f.name)
		}
	}
}

// WithMode sets the file's permission mode (Unix-style) and type bits.
// This affects both the stored metadata and the extracted file's permissions.
// Use fs.FileMode constants like fs.ModeDir, fs.ModePerm, etc.
// The final values stored in zip archive are system dependent.
func WithMode(mode fs.FileMode) AddOption {
	return func(f *File) {
		f.mode = mode
	}
}

// compressorKey defines a key for compressors map
type compressorKey struct {
    method CompressionMethod
    level  int
}

type compressorsMap map[compressorKey]Compressor
type decompressorsMap map[CompressionMethod]Decompressor

// Zip represents an in-memory ZIP archive that can be created, modified, and written.
// It provides thread-safe operations for concurrent access and supports both
// sequential and parallel processing modes.
type Zip struct {
	mu            sync.RWMutex     // Protects concurrent access to archive state
	config        ZipConfig        // Global archive configuration
	files         []*File          // List of files in the archive (including directories)
	fileCache     map[string]bool  // Fast lookup for file existence and type detection
	compressors   compressorsMap   // Registry of custom compressors
	decompressors decompressorsMap // Registry of custom decompressors
	bufferPool    sync.Pool        // Reusable byte buffers for I/O operations
}

// NewZip creates a new empty ZIP archive with default settings.
// The returned archive supports Stored and Deflated compression methods by default.
// Custom compression algorithms can be added using RegisterCompressor and RegisterDecompressor.
// The archive uses a 64KB buffer pool for efficient I/O operations.
func NewZip() *Zip {
	return &Zip{
		files:         make([]*File, 0),
		fileCache:     make(map[string]bool),
		compressors:   make(compressorsMap),
		decompressors: make(decompressorsMap),
		bufferPool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 64*1024) // 64KB buffer
				return &b
			},
		},
	}
}

// GetFiles returns a slice of all files currently stored in the archive.
// The returned slice includes both regular files and directory entries.
// Note: The returned slice is a copy of internal references; modifications to
// File objects may affect archive behavior.
func (z *Zip) GetFiles() []*File {
	return z.files
}

// SetConfig updates the global configuration for the ZIP archive.
// This configuration applies to all subsequently added files unless overridden
// by file-specific options. Thread-safe.
func (z *Zip) SetConfig(c ZipConfig) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.config = c
}

// RegisterCompressor registers a custom compressor implementation for a specific
// compression method and level combination. This is required when reading archives that
// use compression methods not natively supported by the library. Thread-safe.
func (z *Zip) RegisterCompressor(method CompressionMethod, level int, c Compressor) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.compressors[compressorKey{method: method, level: level}] = c
}

// RegisterDecompressor registers a custom decompressor implementation for a
// specific compression method. This is required when reading archives that
// use compression methods not natively supported by the library.
// Thread-safe.
func (z *Zip) RegisterDecompressor(method CompressionMethod, d Decompressor) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.decompressors[method] = d
}

// AddFile adds an existing opened file (os.File) to the archive.
// The file's current position is used for reading; the caller is responsible
// for opening and closing the file. File metadata (size, mod time, etc.) is
// extracted from the os.File handle.
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

// AddFromPath adds a file from the local filesystem to the archive.
// The file is opened, read, and closed automatically. The original filename
// is used unless overridden by WithName or WithPath options.
func (z *Zip) AddFromPath(path string, options ...AddOption) error {
	fileEntry, err := newFileFromPath(path)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
}

// AddFromDir recursively adds all files and directories from a filesystem directory
// to the archive. The directory structure is preserved within the archive.
// The root directory itself is not included; only its contents are added.
// Returns a combined error if any operations failed (Best Effort strategy).
func (z *Zip) AddFromDir(path string, options ...AddOption) error {
	var errs []error

	err := filepath.WalkDir(path, func(walkPath string, _ fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("access error %s: %w", walkPath, err))
			return nil 
		}

		if walkPath == path {
			return nil
		}

		relPath, err := filepath.Rel(path, walkPath)
		if err != nil {
			return err
		}

		pathOpt := WithPath(filepath.ToSlash(filepath.Dir(relPath)))
		fileOpts := append([]AddOption{pathOpt}, options...)

		if err := z.AddFromPath(walkPath, fileOpts...); err != nil {
			errs = append(errs, fmt.Errorf("failed to add %s: %w", walkPath, err))
			return nil
		}
		
		return nil
	})

	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// AddReader adds file content from an arbitrary io.Reader to the archive.
// This is useful for adding dynamically generated content or reading from network streams.
func (z *Zip) AddReader(r io.Reader, filename string, options ...AddOption) error {
	fileEntry, err := newFileFromReader(r, filename)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
}

// CreateDirectory adds an explicit directory entry to the ZIP archive.
// While directories are often created implicitly when files are added with
// path components, this method allows creating empty directories or
// directories with specific metadata (permissions, timestamps).
func (z *Zip) CreateDirectory(name string, options ...AddOption) error {
	dirEntry, err := newDirectoryFile(name)
	if err != nil {
		return fmt.Errorf("create directory file: %w", err)
	}
	return z.addEntry(dirEntry, options)
}

// Exists checks whether a file or directory with the given name exists in the archive.
// The name should use forward slashes as directory separators and will be
// normalized for comparison. Returns true for both exact matches and
// directory matches (e.g., "dir/" matches when checking for "dir").
// Thread-safe for concurrent reads.
func (z *Zip) Exists(name string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	key := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	return z.fileCache[key] || z.fileCache[key+"/"]
}

// Write serializes the entire ZIP archive to the destination writer.
// This method processes files sequentially, making it more memory-efficient
// than WriteParallel for most use cases. If dest implements io.Seeker,
// the implementation can optimize by writing directly without temporary files.
// Files are sorted according to the configured FileSortStrategy before writing.
func (z *Zip) Write(dest io.Writer) error {
	z.mu.RLock()
	files := SortFilesOptimized(z.files, z.config.FileSortStrategy)
	z.mu.RUnlock()

	writer := newZipWriter(z.config, z.compressors, dest)
	for _, file := range files {
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("write file %s: %w", file.name, err)
		}
	}
	return writer.WriteCentralDirAndEndRecords()
}

// WriteParallel serializes the ZIP archive using concurrent workers for compression.
// This method can significantly improve performance on multi-core systems when
// compressing many files. Temporary files and memory buffers are used to store
// compressed data before final sequential writing to maintain ZIP format correctness.
// maxWorkers controls the maximum number of concurrent compression operations.
func (z *Zip) WriteParallel(dest io.Writer, maxWorkers int) error {
	z.mu.RLock()
	files := SortFilesOptimized(z.files, z.config.FileSortStrategy)
	z.mu.RUnlock()

	writer := newParallelZipWriter(z.config, z.compressors, dest, maxWorkers)
	errs := writer.WriteFiles(files)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Read parses an existing ZIP archive from the source and appends its contents
// to the current archive. This allows merging archives or inspecting existing ones.
// The source must be seekable (io.ReadSeeker) for random access to ZIP structures.
// NOTE: This function is thread safe only if source implements [io.ReaderAt].
func (z *Zip) Read(src io.ReadSeeker) error {
	reader := newZipReader(src, z.decompressors)
	files, err := reader.ReadFiles()
	z.files = append(z.files, files...)
	return err
}

// Extract sequentially extracts all files from the archive to the specified directory.
// Directory structure is preserved, and file permissions/timestamps are restored
// when supported by the host filesystem. Path traversal attacks are prevented
// by validating extracted paths. Returns combined errors if any extraction fails.
func (z *Zip) Extract(path string) error {
	path = filepath.Clean(path)
	var errs []error

	z.mu.RLock()
	files := sortAlphabetical(z.files)
	z.mu.RUnlock()

	for _, f := range files {
		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
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
			errs = append(errs, fmt.Errorf("failed to extract %s: %w", fpath, err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// ExtractParallel extracts archive contents using multiple concurrent workers.
// This can significantly improve extraction speed for archives with many files,
// especially on SSDs or fast storage systems. Directory creation is performed
// upfront, then file extraction is parallelized with controlled concurrency.
func (z *Zip) ExtractParallel(path string, workers int) error {
	path = filepath.Clean(path)
	var errs []error

	var filesToExtract []*File
	dirsToCreate := make(map[string]struct{})
	dirsToCreate[path] = struct{}{}

	z.mu.RLock()
	files := sortAlphabetical(z.files)
	z.mu.RUnlock()

	for _, f := range files {
		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
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

		go func(f *File) {
			defer func() { <-sem; wg.Done() }()

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

// addEntry is an internal validator used to add files to archive.
// It applies configuration defaults, processes AddOptions, normalizes the filename,
// checks for duplicates and path collisions, creates implicit directories, and
// updates internal data structures. Thread-safe.
func (z *Zip) addEntry(f *File, options []AddOption) error {
	if !f.isDir {
		f.config.CompressionMethod = z.config.CompressionMethod
		f.config.CompressionLevel = z.config.CompressionLevel
		f.config.EncryptionMethod = z.config.EncryptionMethod
		f.config.Password = z.config.Password
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
		return fmt.Errorf("create missing dirs: %w", err)
	}

	z.files = append(z.files, f)
	z.fileCache[f.getFilename()] = true
	return nil
}

// createMissingDirs ensures that all parent directories for a file path exist
// in the archive by creating implicit directory entries when necessary.
// It traverses the path from bottom to top, creating missing directories and
// checking for file/directory conflicts. Returns an error if a path component
// already exists as a regular file.
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
		dirEntry, err := newDirectoryFile(missingDirs[i])
		if err != nil {
			return err
		}
		z.files = append(z.files, dirEntry)
		z.fileCache[missingDirs[i]+"/"] = true
	}

	return nil
}

// extractFile handles the extraction of a single file from the archive to disk.
// It opens the file from the archive, creates the destination file, copies data
// with buffered I/O, and sets permissions and timestamps. Uses the shared buffer
// pool for efficient memory allocation.
func (z *Zip) extractFile(f *File, path string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(path)
	if err != nil {
		return err
	}
	defer dst.Close()

	if f.uncompressedSize > 0 {
		if err := dst.Truncate(f.uncompressedSize); err != nil {
			return err
		}
	}

	bufPtr := z.bufferPool.Get().(*[]byte)
	_, err = io.CopyBuffer(dst, src, *bufPtr)
	z.bufferPool.Put(bufPtr)

	if err != nil {
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
