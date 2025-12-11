// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SizeUnknown is a sentinel value indicating that the uncompressed size of a file
// cannot be determined in advance. This typically occurs when creating files from
// io.Reader sources where the total data length is unknown until fully read.
const SizeUnknown int64 = -1

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

	// TextEncoding specifies a fallback decoder for filenames/comments
	// when UTF-8 flag is not set.
	// Example: [DecodeIBM866] for Russian DOS archives.
	// Default: CP437 (US DOS).
	TextEncoding TextDecoder

	// OnFileProcessed is called immediately after a file entry is successfully
	// processed in Write, Read and Extract methods and their variants.
	// NOTE: In ExtractParallel this callback is called concurrently.
	OnFileProcessed func(f *File)
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

// ExtractOption represents a function option for configuring extraction.
type ExtractOption func(files []*File) []*File

// WithFiles allows to extract specific files from archive.
// Empty slice results in no files being extracted.
func WithFiles(files []*File) ExtractOption {
	return func(_ []*File) []*File { return files }
}

// FromDir allows to extract files from specific directory. Empty path is ignored.
func FromDir(path string) ExtractOption {
	return func(files []*File) []*File {
		if path == "" || path == "." {
			return files
		}

		dirPath := path
		if !strings.HasSuffix(dirPath, "/") {
			dirPath += "/"
		}

		result := make([]*File, 0, len(files))
		for _, file := range files {
			if strings.HasPrefix(file.name, path) {
				result = append(result, file)
			}
		}
		return result
	}
}

// WithoutDir allows to exclude directory and its contents from extraction.
// Empty path results in all files being excluded.
func WithoutDir(path string) ExtractOption {
	return func(files []*File) []*File {
		if path == "" || path == "." {
			return nil
		}

		dirPath := path
		if !strings.HasSuffix(dirPath, "/") {
			dirPath += "/"
		}

		result := make([]*File, 0, len(files))
		for _, file := range files {
			if !strings.HasPrefix(file.name, dirPath) {
				result = append(result, file)
			}
		}
		return result
	}
}

// Compressor defines a strategy for compressing data.
// See [DeflateCompressor] as an example.
type Compressor interface {
	// Compress reads from src, compresses the data, and writes to dest.
	// Returns the number of uncompressed bytes read from src.
	Compress(src io.Reader, dest io.Writer) (int64, error)
}

// Decompressor defines a strategy for decompressing data.
// See [DeflateDecompressor] as an example.
type Decompressor interface {
	// Decompress returns a ReadCloser that reads uncompressed data from src.
	// src is the stream of compressed data.
	Decompress(src io.Reader) (io.ReadCloser, error)
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
	z.compressors[compressorKey{method, level}] = c
}

// RegisterDecompressor registers a custom decompressor implementation for a
// specific compression method. This is required when reading archives that
// use compression methods not natively supported by the library. Thread-safe.
func (z *Zip) RegisterDecompressor(method CompressionMethod, d Decompressor) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.decompressors[method] = d
}

// GetFiles returns a slice of all files currently stored in the archive.
// The returned slice includes both regular files and directory entries.
// Note: The returned slice is a copy of internal references; modifications to
// File objects may affect archive behavior.
func (z *Zip) GetFiles() []*File {
	return z.files
}

// GetFile returns the file entry with the given name and path. It returns nil if the file is not found.
// The name is automatically normalized to match ZIP standards (forward slashes).
func (z *Zip) GetFile(name string) (*File, error) {
	z.mu.RLock()
	defer z.mu.RUnlock()

	searchName := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")

	if !z.fileCache[searchName] && !z.fileCache[searchName+"/"] {
		return nil, ErrFileNotFound
	}

	for _, f := range z.files {
		target := searchName
		if f.isDir {
			target += "/"
		}

		if f.getFilename() == target {
			return f, nil
		}
	}

	return nil, ErrFileNotFound
}

// GetMatchingFiles returns a list of files matching the glob pattern.
// The pattern syntax is the same as [path.Match].
func (z *Zip) GetMatchingFiles(pattern string) ([]*File, error) {
	z.mu.RLock()
	defer z.mu.RUnlock()

	var matches []*File
	for _, f := range z.files {
		matched, err := path.Match(pattern, f.name)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern: %w", err)
		}
		if matched {
			matches = append(matches, f)
		}
	}
	return matches, nil
}

// Rename updates the file base name leaving the directory path unchanged.
// For example, renaming "logs/current.log" to "old.log" results in "logs/old.log".
// If the entry is a directory, the trailing slash is preserved automatically.
// Returns error if the new name results in a path exceeding ZIP limits.
func (z *Zip) Rename(file *File, newName string) error {
	if newName == "" {
		return fmt.Errorf("%w: new name cannot be empty", ErrFileEntry)
	}

	dir := path.Dir(file.name)
	if dir == "." {
		dir = ""
	}

	fullPath := path.Join(dir, newName)
	fullPath = strings.TrimPrefix(path.Clean(strings.ReplaceAll(fullPath, "\\", "/")), "/")

	if len(fullPath)+1 > math.MaxUint16 {
		return fmt.Errorf("%w: %s (%d bytes)", ErrFilenameTooLong, fullPath, len(fullPath))
	}

	delete(z.fileCache, file.getFilename())
	file.name = fullPath
	z.fileCache[file.getFilename()] = true

	return nil
}

// Move updates the directory path of the file, keeping the base filename.
// For example, moving "logs/app.log" to "backup/2023" results in "backup/2023/app.log".
// Passing "" or "." as newPath moves the file to the archive root.
func (z *Zip) Move(file *File, newPath string) error {
	baseName := path.Base(file.name)

	if file.isDir && baseName == "." {
		baseName = strings.TrimSuffix(file.name, "/")
		baseName = path.Base(baseName)
	}

	fullPath := path.Join(newPath, baseName)
	fullPath = strings.TrimPrefix(path.Clean(strings.ReplaceAll(fullPath, "\\", "/")), "/")

	if len(fullPath)+1 > math.MaxUint16 {
		return fmt.Errorf("%w: %s (%d bytes)", ErrFilenameTooLong, fullPath, len(fullPath))
	}

	delete(z.fileCache, file.getFilename())
	file.name = fullPath
	z.fileCache[file.getFilename()] = true
	return nil
}

// AddFile adds an existing opened file (os.File) to the archive.
// The file's current position is used for reading; the caller is responsible
// for opening and closing the file. File metadata (size, mod time, etc.) is
// extracted from the os.File handle.
func (z *Zip) AddFile(f *os.File, options ...AddOption) error {
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
// If size is [SizeUnknown] it will be calculated during archive writing.
func (z *Zip) AddReader(r io.Reader, filename string, size int64, options ...AddOption) error {
	fileEntry, err := newFileFromReader(r, filename, size)
	if err != nil {
		return err
	}
	return z.addEntry(fileEntry, options)
}

// AddBytes adds a file created from a byte slice.
func (z *Zip) AddBytes(data []byte, filename string, options ...AddOption) error {
	return z.AddReader(bytes.NewReader(data), filename, int64(len(data)), options...)
}

// AddString adds a file created from a string.
func (z *Zip) AddString(content string, filename string, options ...AddOption) error {
	return z.AddReader(strings.NewReader(content), filename, int64(len(content)), options...)
}

// CreateDirectory adds an explicit directory entry to the ZIP archive.
// While directories are often created implicitly when files are added with
// path components, this method allows creating empty directories or
// directories with specific metadata (permissions, timestamps).
func (z *Zip) CreateDirectory(name string, options ...AddOption) error {
	dirEntry, err := newDirectoryFile(name)
	if err != nil {
		return err
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

	key := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")
	return z.fileCache[key] || z.fileCache[key+"/"]
}

// Open returns a ReadCloser for the file with the given name inside the archive.
// Returns ErrFileNotFound if the file does not exist. Thread-safe for concurrent reads.
func (z *Zip) OpenFile(name string) (io.ReadCloser, error) {
	z.mu.RLock()
	defer z.mu.RUnlock()

	searchName := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")

	if !z.fileCache[searchName] {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, name)
	}

	for _, f := range z.files {
		if f.name == searchName && !f.isDir {
			return f.Open()
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrFileNotFound, name)
}

// RemoveFile deletes a file or directory entry from the archive by name.
// If the entry is a directory, this ONLY removes the directory entry itself,
// not its contents (files inside it). To remove a directory and its contents,
// you would need to iterate and remove them individually or use RemoveDir.
// Returns ErrFileNotFound if the entry does not exist.
func (z *Zip) RemoveFile(name string) error {
	z.mu.Lock()
	defer z.mu.Unlock()

	searchName := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")

	isDir := z.fileCache[searchName+"/"]
	if !z.fileCache[searchName] && !isDir {
		return fmt.Errorf("%w: %s", ErrFileNotFound, name)
	}

	idx := -1
	for i, f := range z.files {
		target := searchName
		if f.isDir {
			target += "/"
		}

		if f.getFilename() == target {
			idx = i
			break
		}
	}

	if idx == -1 {
		// Should not happen if cache check passed, but safety first
		return fmt.Errorf("%w: %s", ErrFileNotFound, name)
	}

	z.files = append(z.files[:idx], z.files[idx+1:]...)
	if isDir {
		delete(z.fileCache, searchName+"/")
	} else {
		delete(z.fileCache, searchName)
	}

	return nil
}

// RemoveDir deletes a directory by name with its files and subdirectories.
// If name is an empty strings, all files inside archive are deleted.
// Returns ErrFileNotFound if no deletions were performed.
func (z *Zip) RemoveDir(name string) error {
	if name == "" || name == "." {
		z.files = make([]*File, 0)
		z.fileCache = make(map[string]bool)
		return nil
	}

	searchName := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")
	if !strings.HasSuffix(searchName, "/") {
		searchName += "/"
	}

	newFiles := make([]*File, 0, len(z.files))
	deletedCount := 0

	for _, f := range z.files {
		fileName := f.getFilename()

		// Check if file belongs to the directory (Prefix Match)
		// Since searchName ends with "/", this covers:
		// - The directory entry itself ("foo/bar/")
		// - Files inside ("foo/bar/file.txt")
		// - Subdirectories ("foo/bar/sub/")
		if strings.HasPrefix(fileName, searchName) {
			delete(z.fileCache, fileName)
			deletedCount++
			continue
		}

		newFiles = append(newFiles, f)
	}

	if deletedCount == 0 {
		// Remove the trailing slash for the error message to look natural
		return fmt.Errorf("%w: %s", ErrFileNotFound, strings.TrimSuffix(searchName, "/"))
	}

	z.files = newFiles
	return nil
}

// Write serializes the entire ZIP archive to the destination writer.
// This method processes files sequentially, making it more memory-efficient
// than WriteParallel for most use cases. If dest implements io.Seeker,
// the implementation can optimize by writing directly without temporary files.
// Files are sorted according to the configured FileSortStrategy before writing.
func (z *Zip) Write(dest io.Writer) error {
	return z.WriteWithContext(context.Background(), dest)
}

// WriteWithContext serializes the ZIP archive with context support.
func (z *Zip) WriteWithContext(ctx context.Context, dest io.Writer) error {
	z.mu.RLock()
	files := SortFilesOptimized(z.files, z.config.FileSortStrategy)
	z.mu.RUnlock()

	writer := newZipWriter(z.config, z.compressors, dest)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writer.WriteFile(file); err != nil {
			return fmt.Errorf("zip: write file %s: %w", file.name, err)
		}
		if z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(file)
		}
	}

	if err := writer.WriteCentralDirAndEndRecords(); err != nil {
		return fmt.Errorf("zip: %w", err)
	}

	return nil
}

// WriteParallel serializes the ZIP archive using concurrent workers for compression.
// This method can significantly improve performance on multi-core systems when
// compressing many files. Temporary files and memory buffers are used to store
// compressed data before final sequential writing to maintain ZIP format correctness.
// maxWorkers controls the maximum number of concurrent compression operations.
func (z *Zip) WriteParallel(dest io.Writer, maxWorkers int) error {
	return z.WriteParallelWithContext(context.Background(), dest, maxWorkers)
}

// WriteParallelWithContext serializes the ZIP archive using concurrent workers with context support.
func (z *Zip) WriteParallelWithContext(ctx context.Context, dest io.Writer, maxWorkers int) error {
	z.mu.RLock()
	files := SortFilesOptimized(z.files, z.config.FileSortStrategy)
	z.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	writer := newParallelZipWriter(z.config, z.compressors, dest, maxWorkers)
	errs := writer.WriteFiles(ctx, files)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Read parses a ZIP archive from the source and appends its files to this archive.
func (z *Zip) Read(src io.ReaderAt, size int64) error {
	return z.ReadWithContext(context.Background(), src, size)
}

// ReadWithContext parses a ZIP archive with context cancellation support.
func (z *Zip) ReadWithContext(ctx context.Context, src io.ReaderAt, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	reader := newZipReader(src, size, z.decompressors, z.config)
	files, err := reader.ReadFiles(ctx)
	if err != nil {
		return err
	}
	z.files = append(z.files, files...)
	for _, file := range files {
		z.fileCache[file.getFilename()] = true
	}
	return nil
}

// ReadFile parses an existing ZIP archive and appends its files to this archive.
func (z *Zip) ReadFile(f *os.File) error {
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	return z.Read(f, stat.Size())
}

// Extract sequentially extracts all files from the archive to the specified directory.
// Directory structure is preserved, and file permissions/timestamps are restored
// when supported by the host filesystem. Path traversal attacks are prevented
// by validating extracted paths. Returns combined errors if any extraction fails.
// It expects that given path already exists.
func (z *Zip) Extract(path string, options ...ExtractOption) error {
	return z.ExtractWithContext(context.Background(), path, options...)
}

// ExtractWithContext sequentially extracts files with context support.
// Extraction stops immediately if context is cancelled.
func (z *Zip) ExtractWithContext(ctx context.Context, path string, options ...ExtractOption) error {
	path = filepath.Clean(path)
	var errs []error

	z.mu.RLock()
	files := z.GetFiles()
	for _, opt := range options {
		files = opt(files)
	}
	files = sortAlphabetical(files)
	z.mu.RUnlock()

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}

		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
		fpath := filepath.Join(path, f.name)

		if !strings.HasPrefix(fpath, path+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("%w: %s", ErrInsecurePath, fpath))
			continue
		}

		if f.isDir {
			if err := os.Mkdir(fpath, 0755); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		if err := z.extractFile(ctx, f, fpath); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			errs = append(errs, fmt.Errorf("failed to extract %s: %w", fpath, err))
		} else if z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(f)
		}
	}

	return errors.Join(errs...)
}

// ExtractParallel extracts archive contents using multiple concurrent workers.
// This can significantly improve extraction speed for archives with many files,
// especially on SSDs or fast storage systems. Directory creation is performed
// upfront, then file extraction is parallelized with controlled concurrency.
// Any missing folders in given path will be created automatically.
func (z *Zip) ExtractParallel(path string, workers int, options ...ExtractOption) error {
	return z.ExtractParallelWithContext(context.Background(), path, workers, options...)
}

// ExtractParallelWithContext extracts files concurrently with context cancellation.
func (z *Zip) ExtractParallelWithContext(ctx context.Context, path string, workers int, options ...ExtractOption) error {
	var errs []error
	var filesToExtract []*File
	dirsToCreate := make(map[string]struct{})
	path = filepath.Clean(path)

	z.mu.RLock()
	files := z.GetFiles()
	for _, opt := range options {
		files = opt(files)
	}
	files = sortAlphabetical(files)
	z.mu.RUnlock()

	for _, f := range files {
		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
		fpath := filepath.Join(path, f.name)

		if !strings.HasPrefix(fpath, path+string(os.PathSeparator)) {
			errs = append(errs, fmt.Errorf("%w: %s", ErrInsecurePath, fpath))
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

	if err := ctx.Err(); err != nil {
		return err
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
		select {
		case <-ctx.Done():
			goto Finish
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(f *File) {
			defer func() { <-sem; wg.Done() }()

			fpath := filepath.Join(path, f.name)

			if err := z.extractFile(ctx, f, fpath); err != nil {
				if ctx.Err() == nil {
					errChan <- fmt.Errorf("failed to extract %s: %w", f.name, err)
				}
			} else if z.config.OnFileProcessed != nil {
				if ctx.Err() == nil {
					z.config.OnFileProcessed(f)
				}
			}
		}(f)
	}

Finish:
	wg.Wait()
	close(errChan)

	if err := ctx.Err(); err != nil {
		return err
	}

	for err := range errChan {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// ExtractToWriter extracts a specific file directly to an io.Writer.
// This is useful for streaming extracted content (e.g. to HTTP response)
// without creating temporary files on disk.
func (z *Zip) ExtractToWriter(name string, dest io.Writer) error {
	rc, err := z.OpenFile(name)
	if err != nil {
		return err
	}
	defer rc.Close()

	if _, err := io.Copy(dest, rc); err != nil {
		return fmt.Errorf("extract to writer: %w", err)
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

	if len(f.name)+1 > math.MaxUint16 {
		return fmt.Errorf("%w (%d bytes)", ErrFilenameTooLong, len(f.name))
	}

	if len(f.config.Comment) > math.MaxUint16 {
		return fmt.Errorf("%w (%d bytes)", ErrCommentTooLong, len(f.config.Comment))
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if z.fileCache[f.name] {
		return fmt.Errorf("%w: '%s' already exists", ErrDuplicateEntry, f.name)
	}

	if !f.isDir && z.fileCache[f.name+"/"] {
		return fmt.Errorf("%w: '%s' is already a directory", ErrDuplicateEntry, f.name)
	}

	if err := z.createMissingDirs(f.name); err != nil {
		return err
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
			return fmt.Errorf("%w: '%s' is already a file", ErrDuplicateEntry, dir)
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
// It opens the file from the archive, creates the destination file, copies data with buffered I/O,
// and sets permissions and timestamps. Uses the shared buffer pool for efficient memory allocation.
// It uses contextReader to wrap the source stream, ensuring io.Copy stops if ctx is done.
func (z *Zip) extractFile(ctx context.Context, f *File, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	reader := &contextReader{ctx: ctx, r: src}

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
	_, err = io.CopyBuffer(dst, reader, *bufPtr)
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
