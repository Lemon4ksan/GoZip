// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gozip provides a high-performance, concurrency-safe, and feature-rich
// implementation of the ZIP archive format.
//
// It is designed as a robust alternative to the standard library's archive/zip,
// specifically built for high-load applications, security-conscious environments,
// and scenarios requiring legacy compatibility.
//
// # Key Features
//
// 1. Concurrency: Unlike the standard library, gozip supports parallel compression
// (WriteToParallel) and parallel extraction (ExtractParallel), scaling linearly
// with CPU cores.
//
// 2. Security: Native support for WinZip AES-256 encryption (reading and writing)
// and built-in "Zip Slip" protection during extraction to prevent directory
// traversal attacks.
//
// 3. Context Awareness: All long-running operations support context.Context for
// cancellation and timeout management, making it ideal for HTTP handlers and
// background jobs.
//
// 4. Compatibility: Handles Zip64 (files > 4GB), NTFS timestamps, Unix permissions,
// and legacy DOS encodings (CP437, CP866) automatically.
//
// 5. File System Interface: The archive can be accessed as a read-only filesystem
// using the [fs.FS] interface. This allows seamless integration with Go's standard
// filesystem APIs, such as [io/fs] and [path/filepath].
// Example:
//
//	archive := gozip.NewZip()
//	// ... add files to the archive ...
//	fsys := archive.FS()
//	data, _ := fs.ReadFile(fsys, "file.txt")
//
// # Error handling
//
// [FileError] is used for errors related to [File] instance and operations,
// allowing users to access file metadata (name, size) for logging/retry logic.
// Plain wrapped errors are used for global archive issues.
// Example:
//
//	err := archive.AddFile("data/report.pdf")
//	if err != nil {
//	    var fileErr *gozip.FileError
//	    if errors.As(err, &fileErr) {
//	        fmt.Printf("Operation: %s\n", fileErr.Op)   // e.g., "add", "compress", "extract"
//	        fmt.Printf("File:      %s\n", fileErr.File.Name())
//	        fmt.Printf("Cause:     %v\n", fileErr.Err)  // Underlying error (e.g., [ErrPasswordMismatch])
//	    }
//	}
//
// # Basic Usage
//
// Creating an archive sequentially:
//
//	archive := gozip.NewZip()
//	archive.AddFile("file.txt")
//	archive.AddDir("images/", WithCompression(gozip.Deflate, gozip.DeflateMaximum))
//
//	f, _ := os.Create("output.zip")
//	archive.WriteTo(f)
//
// Creating an archive in parallel (faster for multiple files):
//
//	// compress using 8 workers
//	archive.WriteToParallel(f, 8)
//
// Modifying an existing archive:
//
//	archive := gozip.NewZip()
//	src, _ := os.Open("old.zip")
//	archive.LoadFromFile(src)
//
//	// 1. Remove obsolete files
//	archive.Remove("logs/obsolete.log")
//
//	// 2. Modify a file
//	file, _ := archive.File("data/config.json")
//	archive.Remove(file.Name())
//	archive.AddLazy(file.Name(), func() (io.ReadCloser, error) {
//		pr, pw := io.Pipe()
//		go func() {
//			defer pw.Close()
//			rc, err := file.Open()
//			if err != nil {
//				pw.CloseWithError(err)
//				return
//			}
//			defer rc.Close()
//			processor.Transform(rc, pw)
//		}()
//		return pr, nil
//	})
//
//	// 3. Rename entries
//	archive.Rename("dir/old", "new") // -> dir/new
//
//	// Save changes to a new writer
//	dest, _ := os.Create("new.zip")
//	archive.WriteTo(dest)
//
//	// Close source after the work is done
//	src.Close()
//
// Streaming to HTTP response:
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		w.Header().Set("Content-Type", "application/zip")
//		w.Header().Set("Content-Disposition", `attachment; filename="download.zip"`)
//
//		archive := gozip.NewZip()
//		archive.AddFile("report.pdf")
//
//		// Stream directly to the client
//		archive.WriteTo(w)
//	}
package gozip

import (
	"bytes"
	"context"
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

// SizeUnknown is a sentinel value used when the data size
// cannot be determined before writing (e.g., streaming from [io.Reader]).
const SizeUnknown int64 = -1

// ZipConfig defines global configuration parameters for the archive.
// These settings apply to the entire archive but can be overridden
// per-file using [FileConfig] options.
type ZipConfig struct {
	// CompressionMethod is the default algorithm for new files.
	CompressionMethod CompressionMethod

	// CompressionLevel controls the speed vs size trade-off (0-9).
	// 0 = Store (no compression), 9 = Best compression.
	CompressionLevel int

	// EncryptionMethod is the default encryption algorithm.
	// Recommended: [AES256].
	EncryptionMethod EncryptionMethod

	// Password is the default credentials for encrypting the archive.
	// If specified, defaults to [AES256] encryption.
	Password string

	// Comment is the archive-level comment (max 65535 bytes).
	Comment string

	// FileSortStrategy determines the order of file processing
	// and their order in the written archive.
	FileSortStrategy FileSortStrategy

	// ConflictHandler defines the strategy for handling duplicate file names during [Zip.Load].
	// If nil, defaults to [ActionReplace] (Last Write Wins).
	ConflictHandler ConflictHandler

	// TextEncoding handles filename decoding for legacy archives (non-UTF8).
	// This function is only used in read operations. GoZip always sets
	// the UTF-8 flag for maximum compatibility when writing.
	// Default: [DecodeCP437] (IBM PC).
	TextEncoding TextDecoder

	// OnFileProcessed is a callback triggered after a file is written, read, or extracted.
	// Errors are not wrapped in [FileError], because file instance is passed separately.
	//
	// Advanced Usage (Fail-Fast):
	// This callback can be used to stop bulk operations on the first error.
	// Simply capture a context's cancel function and call it when err != nil.
	// The library will catch the cancellation and perform a graceful shutdown.
	//
	// WARNING: In parallel operations, this callback is triggered concurrently.
	OnFileProcessed func(*File, error)

	// MemoryThreshold determines the maximum file size in bytes that can be buffered in memory.
	// If the file size exceeds this threshold, a temporary file will be used. The default value is 10 MB.
	MemoryThreshold int64

	// UseImplicitDirs determines whether to not create explicit entries for directories,
	// allowing to save disk space and memory. It only affects the created archive structure.
	UseImplicitDirs bool
}

// FileConfig defines configuration specific to a single archive entry.
// It overrides the global [ZipConfig].
type FileConfig struct {
	// CompressionMethod overrides the global default.
	CompressionMethod CompressionMethod

	// CompressionLevel overrides the global default.
	CompressionLevel int

	// EncryptionMethod overrides the global default.
	EncryptionMethod EncryptionMethod

	// Password overrides the global archive password for this file.
	Password string

	// Comment is a file-specific comment (max 65535 bytes).
	Comment string
}

// AddOption is a functional option for configuring file entries during addition.
type AddOption func(f *File)

// WithConfig applies a complete [FileConfig], overwriting existing settings.
func WithConfig(c FileConfig) AddOption {
	return func(f *File) {
		f.SetConfig(c)
	}
}

// WithCompression sets the compression method and level for a regular file.
// Ignored for directories.
func WithCompression(c CompressionMethod, lvl int) AddOption {
	return func(f *File) {
		if !f.isDir {
			f.config.CompressionMethod = c
			f.config.CompressionLevel = lvl
		}
	}
}

// WithEncryption sets the encryption method and password for a regular file.
// Ignored for directories.
func WithEncryption(e EncryptionMethod, pwd string) AddOption {
	return func(f *File) {
		if !f.isDir {
			f.config.EncryptionMethod = e
			f.config.Password = pwd
		}
	}
}

// WithPassword sets the encryption password for a specific file.
// If no encryption method is specified, it defaults to [AES256].
// Ignored for directories.
func WithPassword(pwd string) AddOption {
	return func(f *File) {
		if !f.isDir {
			f.config.Password = pwd
		}
	}
}

// WithName overrides the destination filename within the archive.
// The name is automatically normalized to use forward slashes.
func WithName(name string) AddOption {
	return func(f *File) {
		if name != "" {
			f.name = name
		}
	}
}

// WithPath prepends a directory path to the file's name.
// The path is automatically normalized to use forward slashes.
func WithPath(p string) AddOption {
	return func(f *File) {
		if p != "" && p != "." {
			f.name = path.Join(p, f.name)
		}
	}
}

// WithMode sets the Unix-style permission bits.
// This affects the external attributes field in the ZIP header.
func WithMode(mode fs.FileMode) AddOption {
	return func(f *File) {
		f.mode = mode
	}
}

// Filter filters out the files.
type Filter func(files []*File) []*File

// WithFiles filters the operation to only the specific files provided.
func WithFiles(files []*File) Filter {
	return func(_ []*File) []*File { return files }
}

// FromDir restricts operation to files nested under the specified path.
func FromDir(dirPath string) Filter {
	return func(files []*File) []*File {
		if dirPath == "" || dirPath == "." {
			return files
		}

		prefix := strings.TrimPrefix(path.Clean(strings.ReplaceAll(dirPath, "\\", "/")), "/")
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		dirEntryName := strings.TrimSuffix(prefix, "/")

		n := 0
		for _, f := range files {
			fName := f.entryName()
			if strings.HasPrefix(fName, prefix) || fName == dirEntryName {
				files[n] = f
				n++
			}
		}

		for i := n; i < len(files); i++ {
			files[i] = nil
		}

		return files[:n]
	}
}

// WithoutDir excludes a directory and its contents from operation.
func WithoutDir(dirPath string) Filter {
	return func(files []*File) []*File {
		if dirPath == "" || dirPath == "." {
			return nil
		}

		prefix := strings.TrimPrefix(path.Clean(strings.ReplaceAll(dirPath, "\\", "/")), "/")
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		dirEntryName := strings.TrimSuffix(prefix, "/")

		n := 0
		for _, f := range files {
			fName := f.entryName()
			if strings.HasPrefix(fName, prefix) || fName == dirEntryName {
				continue
			}
			files[n] = f
			n++
		}

		for i := n; i < len(files); i++ {
			files[i] = nil
		}

		return files[:n]
	}
}

// CompressorFactory creates a [Compressor] instance for a specific compression level.
// The level parameter is typically 0-9, but interpretations vary by algorithm.
// Implementations should normalize invalid levels to defaults.
type CompressorFactory func(level int) Compressor

// Compressor transforms raw data into compressed data.
type Compressor interface {
	// Compress reads from src and writes compressed data to dest.
	// Returns the number of uncompressed bytes read.
	Compress(src io.Reader, dest io.Writer) (int64, error)
}

// Decompressor transforms compressed data back into raw data.
type Decompressor interface {
	// Decompress returns a stream of uncompressed data.
	Decompress(src io.Reader) (io.ReadCloser, error)
}

type compressorKey struct {
	method CompressionMethod
	level  int
}

type factoriesMap map[CompressionMethod]CompressorFactory
type compressorsMap map[compressorKey]Compressor
type decompressorsMap map[CompressionMethod]Decompressor

// Zip represents an in-memory ZIP archive manager.
// It is concurrency-safe and supports streaming, random access, and parallel operations.
//
// Thread Safety:
//   - Methods like AddFile, Remove, Exists, File are safe to call concurrently.
//   - However, modifying the archive (Add/Remove) while simultaneously writing it
//     (WriteTo) is NOT supported and may lead to unpredictable results.
//     Finish all modifications before calling WriteTo.
type Zip struct {
	mu            sync.RWMutex     // Guards files, fileCache, and config
	config        ZipConfig        // Global settings
	files         []*File          // List of parsed entries
	lookup        map[string]*File // Lookup map for existence checks (normalized paths)
	factories     factoriesMap     // Factories for creating new compressors (Method -> Factory)
	decompressors decompressorsMap // Registered decompression codecs
	bufferPool    sync.Pool        // Pool of 64KB buffers for IO optimization
}

// NewZip creates a ready-to-use empty ZIP archive.
// Default support includes [Store] (No Compression) and [Deflate].
func NewZip() *Zip {
	return &Zip{
		files:         make([]*File, 0),
		lookup:        make(map[string]*File),
		factories:     make(factoriesMap),
		decompressors: make(decompressorsMap),
		bufferPool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 64*1024) // 64KB
				return &b
			},
		},
	}
}

// Config returns current global zip configuration.
func (z *Zip) Config() ZipConfig {
	return z.config
}

// SetConfig updates the global configuration atomically.
// For files loaded from an existing archive, only the password is applied.
func (z *Zip) SetConfig(c ZipConfig) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.config = c
}

// RegisterCompressor registers a factory function for a specific compression method.
// The factory will be called when a file requires this method at a specific level.
// See [NewDeflateCompressor] and [DeflateCompressor] for optimal implementation example.
func (z *Zip) RegisterCompressor(method CompressionMethod, factory CompressorFactory) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.factories[method] = factory
}

// RegisterDecompressor adds support for reading a custom compression method.
// See [DeflateDecompressor] for implementation example.
func (z *Zip) RegisterDecompressor(method CompressionMethod, d Decompressor) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.decompressors[method] = d
}

// FS returns [fs.FS], a read-only virtual filesystem on top of the ZIP archive.
func (z *Zip) FS() fs.FS {
	return &zipFS{z: z}
}

// AddFile adds a file from the local filesystem to the archive.
//
// Features:
//   - Normalizes paths to use forward slashes.
//   - Stores symlinks as link targets (not followed).
//   - Automatically handles Zip64 for large files.
//
// Conflict Behavior:
//   - Unlike [Zip.Load], this method is strict: if a file with the same name
//     already exists in the archive, it returns [ErrDuplicateEntry] and
//     does NOT overwrite the existing entry.
//   - To replace a file, use [Zip.Remove] before adding.
//
// Options can be used to override compression, encryption, or file attributes.
func (z *Zip) AddFile(path string, options ...AddOption) error {
	fileEntry, err := newFileFromPath(path)
	if err != nil {
		return wrapErr("add", nil, err)
	}
	return wrapErr("add", fileEntry, z.addEntry(fileEntry, options))
}

// AddOSFile adds an open *os.File to the archive. Uses native OS metadata.
// The whole file content will be processed with [io.SectionReader].
func (z *Zip) AddOSFile(f *os.File, options ...AddOption) error {
	fileEntry, err := newFileFromOS(f)
	if err != nil {
		return wrapErr("add", nil, err)
	}
	return wrapErr("add", fileEntry, z.addEntry(fileEntry, options))
}

// AddDir recursively adds a directory and its contents to the archive.
//
// Behavior:
//   - Files are added using "Best Effort" strategy: if a single file fails to read,
//     AddDir continues processing others but returns a joined error at the end.
//   - Symlinks inside the directory are stored as links, not followed.
//   - Use [WithoutDir] or [FromDir] options during extraction, not here.
func (z *Zip) AddDir(path string, options ...AddOption) error {
	var errs []error

	walkErr := filepath.WalkDir(path, func(walkPath string, _ fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, wrapErr("add", nil, fmt.Errorf("scan %s: %w", walkPath, err)))
			return nil
		}

		if walkPath == path {
			return nil
		}

		relPath, err := filepath.Rel(path, walkPath)
		if err != nil {
			errs = append(errs, wrapErr("add", nil, err))
		}

		pathOpt := WithPath(filepath.ToSlash(filepath.Dir(relPath)))
		fileOpts := append([]AddOption{pathOpt}, options...)

		if err := z.AddFile(walkPath, fileOpts...); err != nil {
			errs = append(errs, err)
			return nil
		}

		return nil
	})

	if walkErr != nil {
		errs = append(errs, wrapErr("add", nil, walkErr))
	}

	return errors.Join(errs...)
}

// AddFS adds files from an [fs.FS] (e.g., [embed.FS], [os.DirFS]) to the archive.
// It recursively walks the file system and adds all entries.
func (z *Zip) AddFS(fileSystem fs.FS, options ...AddOption) error {
	return fs.WalkDir(fileSystem, ".", func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filePath == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		pathOpt := WithPath(path.Dir(filePath))

		fileOpts := append([]AddOption{pathOpt}, options...)

		if d.IsDir() {
			return z.Mkdir(filePath, fileOpts...)
		}

		fileEntry, err := newFileFromFS(fileSystem, filePath, info)
		if err != nil {
			return wrapErr("create", fileEntry, err)
		}

		return wrapErr("add", fileEntry, z.addEntry(fileEntry, options))
	})
}

// AddReader appends a file from an [io.Reader] stream.
//
// Parameters:
//   - filename: The name of the entry in the archive.
//   - size: The uncompressed size of the stream. Use [SizeUnknown] if not known.
//
// Performance Warning:
//   - If size is [SizeUnknown] and the target writer is an [io.Seeker] (e.g., os.File),
//     the writer will buffer the entire stream to a temporary file to calculate
//     headers before writing. To avoid this, provide the exact size if possible.
//
// Returns [ErrFileEntry] if an invalid argument is passed.
func (z *Zip) AddReader(r io.Reader, filename string, size int64, options ...AddOption) error {
	fileEntry, err := newFileFromReader(r, filename, size)
	if err != nil {
		return wrapErr("add", nil, err)
	}
	return wrapErr("add", fileEntry, z.addEntry(fileEntry, options))
}

// AddLazy adds a file entry whose content is opened only when writing the archive.
//
// Use Case:
//   - Useful for streaming generated content (PDFs, images) or when files might
//     change between adding them to the struct and writing the archive.
//
// Concurrency Warning:
//   - If [Zip.WriteToParallel] is used, openFunc will be called concurrently.
//     Ensure the closure is thread-safe.
//
// Resource Management:
//   - The [io.ReadCloser] returned by openFunc is automatically closed by the library
//     after the file is written. You do not need to wrap it to close it manually,
//     but you are responsible for closing any resources used to create that reader
//     (e.g. database connections) inside the closure or after WriteTo finishes.
//
// Returns [ErrFileEntry] if an invalid name is passed.
func (z *Zip) AddLazy(name string, openFunc func() (io.ReadCloser, error), options ...AddOption) error {
	fileEntry, err := newFileFromReader(io.LimitReader(nil, 0), name, SizeUnknown)
	if err != nil {
		return wrapErr("add", nil, err)
	}
	fileEntry.openFunc = openFunc
	return wrapErr("add", fileEntry, z.addEntry(fileEntry, options))
}

// AddBytes creates a file from a byte slice.
// See [Zip.AddReader] for full documentation.
func (z *Zip) AddBytes(data []byte, filename string, options ...AddOption) error {
	return z.AddReader(bytes.NewReader(data), filename, int64(len(data)), options...)
}

// AddString creates a file from a string.
// See [Zip.AddReader] for full documentation.
func (z *Zip) AddString(content string, filename string, options ...AddOption) error {
	return z.AddReader(strings.NewReader(content), filename, int64(len(content)), options...)
}

// Mkdir creates an explicit directory entry in the archive.
// Note: Directories are created implicitly by file paths;
// this is used for empty directories or specific metadata.
// Returns [ErrFileEntry] if invalid name is passed.
func (z *Zip) Mkdir(name string, options ...AddOption) error {
	dirEntry, err := newDirectoryFile(name)
	if err != nil {
		return wrapErr("add", nil, err)
	}
	return wrapErr("add", dirEntry, z.addEntry(dirEntry, options))
}

// Remove deletes a file or directory from the archive.
//
// Behavior:
//   - If the target is a directory, it recursively removes all its contents
//     and the directory entry itself.
//   - Providing an empty string or "." results in a "Reset" operation:
//     all entries are removed, and the archive becomes empty.
//   - Path normalization is applied (e.g., "dir\\file.txt" becomes "dir/file.txt").
//
// Errors:
//   - Returns a [FileError] wrapping [ErrFileNotFound] if no entries
//     matched the provided name.
//
// Complexity: O(N) where N is the total number of files in the archive.
func (z *Zip) Remove(name string) error {
	z.mu.Lock()
	defer z.mu.Unlock()

	if name == "" || name == "." {
		if len(z.files) == 0 {
			return nil
		}
		z.files = z.files[:0]
		clear(z.lookup)
		return nil
	}

	cleanName := z.normalizePath(name)
	dirPrefix := cleanName + "/"

	var n, deletedCount int
	for _, f := range z.files {
		fName := f.entryName()

		isExactMatch := fName == cleanName || fName == dirPrefix
		isChild := strings.HasPrefix(fName, dirPrefix)

		if isExactMatch || isChild {
			delete(z.lookup, fName)
			deletedCount++
			continue
		}

		z.files[n] = f
		n++
	}

	if deletedCount == 0 {
		return wrapErr("remove", nil, fmt.Errorf("%w: '%s'", ErrFileNotFound, name))
	}

	for i := n; i < len(z.files); i++ {
		z.files[i] = nil
	}

	z.files = z.files[:n]

	return nil
}

// Rename changes the name of an entry while preserving its current directory location.
// Example: Rename("logs/old.txt", "new.txt") -> "logs/new.txt"
//
// Behavior:
//   - For Directories: Recursively renames all nested files and subdirectories
//     to reflect the new parent name.
//   - Atomicity: This operation is atomic. It performs a "dry run" check of all
//     resulting paths. If any path (including children) exceeds ZIP limits
//     or conflicts with existing entries, no changes are applied to the archive.
//   - Pathing: The 'new' parameter should be the new base name, not a full path.
//
// Errors:
//   - Returns [ErrFileEntry] if the newName is empty or contains slashes.
//   - Returns [ErrFileNotFound] if the old entry does not exist.
//   - Returns [ErrDuplicateEntry] if the destination path is already occupied.
//   - Returns [ErrFilenameTooLong] if any resulting path exceeds 65,535 bytes.
func (z *Zip) Rename(old, newName string) error {
	if old == newName {
		return nil
	}
	if newName == "" {
		return wrapErr("rename", nil, fmt.Errorf("%w: new name cannot be empty", ErrFileEntry))
	}
	if strings.ContainsAny(newName, "\\/") {
		return wrapErr("rename", nil, fmt.Errorf("%w: name cannot contain slashes", ErrFileEntry))
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	file, err := z.findEntry(old)
	if err != nil {
		return wrapErr("rename", nil, err)
	}

	parent := path.Dir(file.name)
	if parent == "." {
		parent = ""
	}
	newPath := path.Join(parent, z.normalizePath(newName))

	return z.atomicPathTransform("rename", file, newPath)
}

// Move changes the directory location of an entry while preserving its base name.
// Example: Move("file.txt", "backup/docs") -> "backup/docs/file.txt".
//
// Behavior:
//   - For Directories: Moves the entire directory tree recursively to the new location.
//   - Implicit Creation: Automatically creates any missing parent directories
//     in the destination path.
//   - Atomicity: Like Rename, this operation is atomic. It validates all
//     resulting child paths before modifying the archive structure.
//
// Errors returned match [Zip.Rename].
func (z *Zip) Move(old, newDir string) error {
	z.mu.Lock()
	defer z.mu.Unlock()

	file, err := z.findEntry(old)
	if err != nil {
		return wrapErr("move", nil, err)
	}
	newPath := path.Join(z.normalizePath(newDir), path.Base(file.name))

	return z.atomicPathTransform("move", file, newPath)
}

// File returns the entry matching the given name.
//
// Complexity: O(1)
//
// Behavior:
//   - Name is case-sensitive (unless specific sorting/normalization is applied).
//   - Handles path normalization (e.g., "dir\file" -> "dir/file").
//   - Returns false if no exact match is found.
func (z *Zip) File(name string) (*File, bool) {
	z.mu.RLock()
	defer z.mu.RUnlock()

	key := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")
	if f, ok := z.lookup[key]; ok {
		return f, true
	}

	if f, ok := z.lookup[key+"/"]; ok {
		return f, true
	}

	return nil, false
}

// Files returns a copy of the list of files in the archive.
func (z *Zip) Files() []*File {
	z.mu.RLock()
	defer z.mu.RUnlock()
	result := make([]*File, len(z.files))
	copy(result, z.files)
	return result
}

// Exists checks if a file or directory exists in the archive.
// Returns true if an exact file match is found.
func (z *Zip) Exists(name string) bool {
	z.mu.RLock()
	defer z.mu.RUnlock()

	key := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")
	_, ok := z.lookup[key]
	if ok {
		return true
	}
	_, ok = z.lookup[key+"/"]
	return ok
}

// OpenFile returns a ReadCloser for the named file within the archive.
// Returns [ErrFileNotFound] if not found or target is a directory.
func (z *Zip) OpenFile(name string) (io.ReadCloser, error) {
	z.mu.RLock()
	defer z.mu.RUnlock()

	f, ok := z.lookup[z.normalizePath(name)]
	if !ok || f.isDir {
		return nil, wrapErr("open", nil, ErrFileNotFound)
	}
	return f.Open()
}

// Glob returns all files file whose name matches the [path.Match] pattern.
func (z *Zip) Glob(pattern string) ([]*File, error) {
	pattern = strings.ReplaceAll(pattern, "\\", "/")

	if _, err := path.Match(pattern, ""); err != nil {
		return nil, err
	}

	if !hasMeta(pattern) {
		if f, ok := z.File(pattern); ok {
			return []*File{f}, nil
		}
		return nil, nil
	}

	z.mu.RLock()
	defer z.mu.RUnlock()

	var matches []*File

	for _, f := range z.files {
		if matched, _ := path.Match(pattern, f.name); matched {
			matches = append(matches, f)
		}
	}

	return matches, nil
}

// Find searches for files matching the pattern in all directories.
// Unlike Glob, the pattern "*" matches "/" characters.
// Example: Find("*.log") matches "error.log" AND "var/logs/access.log".
func (z *Zip) Find(pattern string) ([]*File, error) {
	pattern = strings.ReplaceAll(pattern, "\\", "/")

	if _, err := path.Match(pattern, ""); err != nil {
		return nil, err
	}

	z.mu.RLock()
	defer z.mu.RUnlock()

	var matches []*File
	for _, f := range z.files {
		if matched, _ := path.Match(pattern, path.Base(f.name)); matched {
			matches = append(matches, f)
		}
	}

	return matches, nil
}

// WriteTo serializes the archive to the specified writer sequentially.
//
// Behavior:
//   - Processes files using "Best Effort" strategy.
//   - Writes Central Directory and End of Central Directory (EOCD) records.
//   - Automatically handles Zip64 if files exceed 4GB or 65535 count.
//
// Returns the total number of bytes written.
func (z *Zip) WriteTo(dest io.Writer, filters ...Filter) (int64, error) {
	return z.WriteToWithContext(context.Background(), dest, filters...)
}

// WriteToWithContext writes the archive with context support.
// Cancelling the context stops processing the remaining files and results in a valid archive.
func (z *Zip) WriteToWithContext(ctx context.Context, dest io.Writer, filters ...Filter) (int64, error) {
	files := z.Files()
	for _, filter := range filters {
		files = filter(files)
	}
	files = SortFilesOptimized(files, z.config.FileSortStrategy)

	counter := &byteCountWriter{dest: dest}

	var writerDest io.Writer = counter

	if seeker, ok := dest.(io.WriteSeeker); ok {
		writerDest = &byteCountWriteSeeker{
			byteCountWriter: counter,
			seeker:          seeker,
		}
	}

	writer := newZipWriter(z.config, z.factories, writerDest)

	var errs []error
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}

		err := writer.WriteFile(file)
		if err != nil {
			errs = append(errs, wrapErr("write", file, err))
		}

		if z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(file, err)
		}
	}

	if err := writer.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, fmt.Errorf("zip: finalize: %w", err))
	}

	return counter.bytesWritten, errors.Join(errs...)
}

// WriteToParallel writes the archive using multiple concurrent workers.
//
// Performance:
//   - Scales linearly with CPU cores for compression-heavy tasks (Deflate/AES).
//   - Uses a "Fan-out / Fan-in" pattern to ensure data is written to dest
//     in the correct order, preserving deterministic output.
//
// Memory Usage:
//   - Workers use a shared buffer pool. Peak memory usage is primarily governed by
//     the number of in-flight files (maxWorkers * 2) and the MemoryThreshold (default 10MB).
//   - Estimated peak RAM: ~ (MaxWorkers * 2 * MemoryThreshold).
//   - Actual usage is often higher as the Go Garbage Collector does not reclaim
//     memory from pools immediately.
//   - Efficiency Note: Processing files in descending order of size ([SortSizeDescending])
//     significantly reduces memory peaks by reusing large buffers more effectively, but it's usually slower.
//   - For memory-constrained environments, reduce maxWorkers or MemoryThreshold.
//
// Returns the total bytes written to dest.
func (z *Zip) WriteToParallel(dest io.Writer, maxWorkers int, filters ...Filter) (int64, error) {
	return z.WriteToParallelWithContext(context.Background(), dest, maxWorkers, filters...)
}

// WriteToParallelWithContext writes concurrently with context support.
// Cancelling the context stops processing the remaining files and results in a valid archive.
func (z *Zip) WriteToParallelWithContext(ctx context.Context, dest io.Writer, maxWorkers int, filters ...Filter) (int64, error) {
	files := z.Files()
	for _, filter := range filters {
		files = filter(files)
	}
	files = SortFilesOptimized(files, z.config.FileSortStrategy)

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	counter := &byteCountWriter{dest: dest}
	writer := newParallelZipWriter(z.config, z.factories, counter, maxWorkers)

	errs := writer.WriteFiles(ctx, files)

	if err := writer.zw.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, err)
	}

	return counter.bytesWritten, errors.Join(errs...)
}

// Load parses an existing ZIP archive's central directory and merges
// its entries into the current Zip instance using "Best Effort" strategy.
// For sources that don't support io.ReaderAt see [StreamReader].
//
// Behavior:
//   - It does not load file contents into memory, only headers.
//   - The current [ZipConfig.Password] is applied to all loaded files.
//
// Conflict Handling (Smart Merge):
//   - If the current archive already contains entries with the same name as in the source,
//     the existing entries are replaced by the new ones by default.
//     This ensures the final archive remains valid (no duplicate file headers).
//   - You can change logic by specifying custom [ZipConfig.ConflictHandler].
//
// Supported Formats:
//   - Standard ZIP, Zip64, and archives with preambles (e.g., self-extracting EXEs).
//
// Errors:
//   - Returns [ErrFormat] if the source is not a valid ZIP archive.
func (z *Zip) Load(src io.ReaderAt, size int64) error {
	return z.LoadWithContext(context.Background(), src, size)
}

// LoadWithContext parses an archive with context support.
// Cancelling the context stops processing the remaining files.
func (z *Zip) LoadWithContext(ctx context.Context, src io.ReaderAt, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	reader := newZipReader(src, size, z.decompressors, z.config)
	eocd, err := reader.FindAndReadEOCD(ctx)
	if err != nil {
		return err
	}

	if z.config.Comment == "" {
		z.config.Comment = eocd.Comment
	}

	files, err := reader.ReadFiles(ctx, eocd)
	if err != nil {
		return err
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	z.prepareInternalStorage(len(files))

	handler := z.config.ConflictHandler
	if handler == nil {
		handler = DefaultConflictHandler
	}

	var errs []error
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}

		file.config.Password = z.config.Password

		added, err := z.addLoadedFile(file, handler)
		if err != nil {
			errs = append(errs, wrapErr("load", file, err))
		}

		if added && z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(file, err)
		}
	}

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// LoadFromFile parses a ZIP from a local os.File.
// See [Zip.Load] for for full documentation.
func (z *Zip) LoadFromFile(f *os.File) error {
	return z.LoadFromFileWithContext(context.Background(), f)
}

// LoadFromFile parses a ZIP from a local os.File with context support.
// See [Zip.LoadWithContext] for for full documentation.
func (z *Zip) LoadFromFileWithContext(ctx context.Context, f *os.File) error {
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	return z.LoadWithContext(ctx, f, stat.Size())
}

// Extract unpacks the archive to the specified destination directory using "Best Effort" strategy.
//
// Security (Zip Slip):
//   - Protects against directory traversal attacks. Attempts to extract files
//     outside the target directory (e.g., "../../../etc/passwd") will result
//     in [ErrInsecurePath].
//
// Features:
//   - Restores file modification times and permissions (chmod).
//   - Automatically creates missing directory structures.
//   - Supports decryption if passwords are set in [ZipConfig] or per-file.
func (z *Zip) Extract(path string, filters ...Filter) error {
	return z.ExtractWithContext(context.Background(), path, filters...)
}

// ExtractWithContext extracts files with context support.
// Context cancellation stops the extraction process.
func (z *Zip) ExtractWithContext(ctx context.Context, path string, filters ...Filter) error {
	path = filepath.Clean(path)
	var errs []error
	var dirsToRestore []*File

	files := z.Files()
	for _, filter := range filters {
		files = filter(files)
	}
	sortAlphabetical(files)

	callback := func(f *File, err error) {
		if z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(f, err)
		}
	}

	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
		fpath := filepath.Join(path, f.name)

		// Zip Slip Protection
		rel, err := filepath.Rel(path, fpath)
		if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			errs = append(errs, wrapErr("extract", f, ErrInsecurePath))
			callback(f, ErrInsecurePath)
			continue
		}

		if f.isDir {
			err := os.MkdirAll(fpath, 0755)
			if err != nil {
				errs = append(errs, wrapErr("extract", f, err))
			} else {
				dirsToRestore = append(dirsToRestore, f)
			}
			callback(f, err)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			errs = append(errs, wrapErr("extract", f, err))
			callback(f, err)
			continue
		}

		err = z.extractFile(ctx, f, fpath)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, ErrPasswordMismatch) {
				f.config.Password = ""
			}
			errs = append(errs, wrapErr("extract", f, err))
		}
		callback(f, err)
	}

	for i := len(dirsToRestore) - 1; i >= 0; i-- {
		dir := dirsToRestore[i]
		os.Chtimes(filepath.Join(path, dir.name), time.Now(), dir.modTime)
	}

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ExtractParallel unpacks files using concurrent workers.
//
// Use Case:
//   - Recommended for SSDs or systems with high I/O throughput.
//   - faster than [Zip.Extract] for archives with many encrypted or compressed files
//     due to parallel CPU decryption/decompression.
func (z *Zip) ExtractParallel(path string, workers int, filters ...Filter) error {
	return z.ExtractParallelWithContext(context.Background(), path, workers, filters...)
}

// ExtractParallelWithContext extracts files concurrently with context support.
func (z *Zip) ExtractParallelWithContext(ctx context.Context, path string, workers int, filters ...Filter) error {
	path = filepath.Clean(path)

	files := z.Files()
	for _, filter := range filters {
		files = filter(files)
	}
	sortAlphabetical(files)

	filesToExtract := make([]*File, 0, len(files))
	dirsToRestore := make([]*File, 0, len(files)/2)

	callback := func(f *File, err error) {
		if z.config.OnFileProcessed != nil {
			z.config.OnFileProcessed(f, err)
		}
	}

	var errs []error
	for _, f := range files {
		if f.config.Password == "" {
			f.config.Password = z.config.Password
		}
		fpath := filepath.Join(path, f.name)

		// Zip Slip Protection
		rel, err := filepath.Rel(path, fpath)
		if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			errs = append(errs, wrapErr("extract", f, ErrInsecurePath))
			if z.config.OnFileProcessed != nil {
				z.config.OnFileProcessed(f, err)
			}
			continue
		}

		if f.isDir {
			if err := os.MkdirAll(fpath, 0755); err != nil {
				errs = append(errs, wrapErr("extract", f, err))
			} else {
				dirsToRestore = append(dirsToRestore, f)
			}
			continue
		}

		filesToExtract = append(filesToExtract, f)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
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

			err := z.extractFile(ctx, f, filepath.Join(path, f.name))
			if err != nil {
				if ctx.Err() == nil {
					errChan <- wrapErr("extract", f, err)
				}
				if errors.Is(err, ErrPasswordMismatch) {
					f.config.Password = ""
				}
			}
			callback(f, err)
		}(f)
	}

Finish:
	wg.Wait()
	close(errChan)

	for i := len(dirsToRestore) - 1; i >= 0; i-- {
		dir := dirsToRestore[i]
		os.Chtimes(filepath.Join(path, dir.name), time.Now(), dir.modTime)
	}

	for err := range errChan {
		errs = append(errs, err)
	}

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Internal helpers

// addEntry validates and adds a file to the archive.
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

	if f.config.Password != "" && f.config.EncryptionMethod == NotEncrypted {
		f.config.EncryptionMethod = AES256
	}

	f.name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(f.name, "\\", "/")), "/")

	if len(f.entryName()) > MaxStringLength {
		return fmt.Errorf("%w (%d bytes)", ErrFilenameTooLong, len(f.name))
	}

	if len(f.config.Comment) > MaxStringLength {
		return fmt.Errorf("%w (%d bytes)", ErrCommentTooLong, len(f.config.Comment))
	}

	z.mu.Lock()
	defer z.mu.Unlock()

	if _, ok := z.lookup[f.name]; ok {
		return fmt.Errorf("%w: file already exists", ErrDuplicateEntry)
	}

	if _, ok := z.lookup[f.name+"/"]; !f.isDir && ok {
		return fmt.Errorf("%w: directory already exists", ErrDuplicateEntry)
	}

	if err := z.createMissingDirs(f.name); err != nil {
		return err
	}

	z.files = append(z.files, f)
	z.lookup[f.entryName()] = f
	return nil
}

// createMissingDirs ensures implicit parent directories exist.
func (z *Zip) createMissingDirs(filePath string) error {
	dir := path.Dir(filePath)
	if dir == "." || dir == "/" {
		return nil
	}
	if _, ok := z.lookup[dir+"/"]; ok {
		return nil
	}

	var missingDirs []string
	for dir != "." && dir != "/" {
		if _, ok := z.lookup[dir+"/"]; ok {
			break
		}
		if _, ok := z.lookup[dir]; ok {
			return fmt.Errorf("%w: %s", ErrDuplicateEntry, dir)
		}
		missingDirs = append(missingDirs, dir)
		dir = path.Dir(dir)
	}

	for i := len(missingDirs) - 1; i >= 0; i-- {
		dirEntry, err := newDirectoryFile(missingDirs[i])
		if err != nil {
			return err
		}

		dirEntry.isImplicit = true

		z.files = append(z.files, dirEntry)
		z.lookup[missingDirs[i]+"/"] = dirEntry
	}

	return nil
}

// normalizePath ensures the name matches the expected archive format.
func (z *Zip) normalizePath(name string) string {
	if name == "" {
		return ""
	}

	// Check whether any changes are needed at all (Zero-alloc check)
	needsFix := false
	for i := 0; i < len(name); i++ {
		char := name[i]
		if char == '\\' || (char == '/' && i+1 < len(name) && name[i+1] == '/') {
			needsFix = true
			break
		}
	}

	// If the path is clean (95% of the time in ZIP this is the case), just check the edges
	if !needsFix {
		res := name
		if len(res) > 0 && res[0] == '/' {
			res = res[1:]
		}
		return res
	}

	// The hard path (for "bad" paths only)
	return strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "/")
}

// findEntry - internal search without blocking.
func (z *Zip) findEntry(name string) (*File, error) {
	key := z.normalizePath(name)
	if f, ok := z.lookup[key]; ok {
		return f, nil
	}
	if f, ok := z.lookup[key+"/"]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("%w: '%s'", ErrFileNotFound, name)
}

// atomicPathTransform ensures that either all files (including children) are moved, or none.
func (z *Zip) atomicPathTransform(op string, f *File, newPath string) error {
	if f.name == newPath {
		return nil
	}

	if err := z.checkPathTransform(op, f, newPath); err != nil {
		return err
	}

	if err := z.createMissingDirs(newPath); err != nil {
		return wrapErr(op, f, err)
	}

	return z.applyPathTransform(f, newPath)
}

// checkPathTransform ensures newPath is valid.
func (z *Zip) checkPathTransform(op string, f *File, newPath string) error {
	if _, exists := z.lookup[newPath]; exists {
		return wrapErr(op, f, fmt.Errorf("%w: '%s'", ErrDuplicateEntry, newPath))
	}
	if _, exists := z.lookup[newPath+"/"]; exists {
		return wrapErr(op, f, fmt.Errorf("%w: '%s'", ErrDuplicateEntry, newPath))
	}

	// Check: Cannot move a folder to itself or to a subfolder
	if f.isDir {
		if strings.HasPrefix(newPath+"/", f.entryName()) {
			return wrapErr(op, f, fmt.Errorf("%w: cannot move directory into itself", ErrFileEntry))
		}
	}

	oldPrefix := f.entryName()
	newPrefix := newPath
	if f.isDir {
		newPrefix += "/"
	}

	for _, entry := range z.files {
		if after, ok := strings.CutPrefix(entry.entryName(), oldPrefix); ok {
			if len(newPrefix+after) > MaxStringLength {
				return wrapErr(op, entry, ErrFilenameTooLong)
			}
		}
	}
	return nil
}

func (z *Zip) applyPathTransform(f *File, newPath string) error {
	oldPrefix := f.entryName()
	newPrefix := newPath
	if f.isDir {
		newPrefix += "/"
	}

	for _, entry := range z.files {
		filename := entry.entryName()
		if after, ok := strings.CutPrefix(filename, oldPrefix); ok {
			delete(z.lookup, filename)
			entry.name = strings.TrimSuffix(newPrefix+after, "/")
			z.lookup[entry.entryName()] = entry
		}
	}

	return nil
}

// prepareInternalStorage optimizes allocations if the archive was empty.
func (z *Zip) prepareInternalStorage(newFilesCount int) {
	if len(z.files) == 0 {
		z.files = make([]*File, 0, newFilesCount)
	}
	if len(z.lookup) == 0 {
		z.lookup = make(map[string]*File, newFilesCount)
	}
}

// addLoadedFile determines how to add a file from an external source to the current archive.
// It returns true if the file was added or replaced, and false if it was skipped.
func (z *Zip) addLoadedFile(file *File, handler ConflictHandler) (bool, error) {
	filename := file.entryName()
	existing, exists := z.lookup[filename]

	if !exists {
		if err := z.createMissingDirs(file.name); err != nil {
			return false, err
		}
		z.lookup[filename] = file
		z.files = append(z.files, file)
		return true, nil
	}

	action, newName := handler(existing, file)

	switch action {
	case ActionReplace:
		z.replaceEntry(existing, file)
		return true, nil

	case ActionSkip:
		return false, nil

	case ActionError:
		return false, ErrDuplicateEntry

	case ActionRename:
		if newName == "" {
			return false, fmt.Errorf("%w: empty rename name", ErrFileEntry)
		}
		file.name = newName
		if err := z.createMissingDirs(file.name); err != nil {
			return false, err
		}
		if _, exists := z.lookup[file.entryName()]; exists {
			return false, fmt.Errorf("%w: rename target '%s' exists", ErrDuplicateEntry, newName)
		}
		z.lookup[file.entryName()] = file
		z.files = append(z.files, file)
		return true, nil

	default:
		return false, nil
	}
}

// replaceEntry atomically replaces an existing file with a new one
// to maintain archive consistency (Last Write Wins).
func (z *Zip) replaceEntry(old, new *File) {
	z.lookup[new.entryName()] = new
	for i, f := range z.files {
		if f == old {
			z.files[i] = new
			return
		}
	}
	z.files = append(z.files, new)
}

// extractFile handles low-level extraction logic.
// It uses the shared buffer pool and attempts to restore file metadata (times/perms).
func (z *Zip) extractFile(ctx context.Context, f *File, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if strings.ContainsAny(path, "\x00\r\n\t") {
		return fmt.Errorf("%w: filename contains control characters", ErrFileEntry)
	}

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

		bufPtr := z.bufferPool.Get().(*[]byte)
		_, err = io.CopyBuffer(dst, &contextReader{ctx: ctx, r: src}, *bufPtr)
		z.bufferPool.Put(bufPtr)

		if err != nil {
			return err
		}
	}

	perm := f.mode & fs.ModePerm
	if perm == 0 {
		perm = 0644
	}
	// Best-effort attempts to restore metadata. Errors are ignored as they
	// may occur on file systems that don't support these operations.
	_ = os.Chmod(path, perm)
	_ = os.Chtimes(path, time.Now(), f.modTime)

	return nil
}
