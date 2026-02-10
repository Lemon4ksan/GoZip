// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gozip provides a high-performance, concurrency-safe, and feature-rich
// implementation of the ZIP archive format.
//
// It is designed as a robust alternative to the standard library's archive/zip,
// specifically built for high-load applications, security-conscious environments,
// and developer experience.
//
// # Key Features
//
// 1. Architecture: The library uses the [Archiver] to encapsulate configuration
// (passwords, codecs). No global state or side effects.
//
// 2. Usability: Includes high-level helpers ([ArchiveDir], [Unzip], [Diff]) for
// common tasks, reducing boilerplate code to the minimum.
//
// 3. Concurrency: Supports parallel compression and extraction, utilizing all
// available CPU cores for maximum throughput using [WithWorkers].
//
// 4. Security: Native support for WinZip AES-256 encryption and built-in "Zip Slip"
// protection during extraction to prevent directory traversal attacks.
//
// 5. Abstraction: Uses [Source] and [Sink] interfaces to transparently handle
// files on disk, in-memory buffers, or network streams.
//
// 6. Context Awareness: All long-running operations support [context.Context] for
// cancellation and timeout management.
//
// 7. Compatibility: Handles Zip64 (files > 4GB), NTFS timestamps, Unix permissions,
// and legacy DOS encodings (CP437, CP866) automatically.
//
// # Quick Start
//
// The simplest way to use the library is via global helper functions that use
// default settings (Deflate compression):
//
//	// Archive a directory
//	err := gozip.ArchiveDir("data/", gozip.ToFilePath("backup.zip"))
//
//	// Extract an archive
//	err := gozip.Unzip(gozip.FromFilePath("backup.zip"), "restored/")
//
//	// Read a single file contents without extraction
//	data, err := gozip.ReadFile(gozip.FromFilePath("config.zip"), "settings.json")
//
// # Advanced Usage (The Archiver)
//
// For custom configuration (passwords, specific compression algorithms), create
// an [Archiver] instance. This allows you to isolate settings per operation.
//
//	// Configure an environment
//	archiver := gozip.NewArchiver(
//	    gozip.WithArchivePassword("secure-password"),
//	    // gozip.WithCompressor(gozip.Zstd, zstd.NewFactory()), // If using custom codecs
//	)
//
//	// Use the configured instance
//	err := archiver.Unzip(gozip.FromFilePath("encrypted.zip"), "output/")
//
// # Source & Sink
//
// The library abstracts IO operations. You can work with physical files,
// byte slices, or streams seamlessly:
//
//	// Unzip from memory (e.g., uploaded file)
//	src := gozip.FromReader(bytes.NewReader(data), int64(len(data)))
//	gozip.Unzip(src, "uploads/")
//
//	// Archive directly to an HTTP response
//	dest := gozip.ToWriter(httpResponseWriter)
//	gozip.ArchiveDir("report/", dest)
//
// # Error handling
//
// [FileError] is used for errors related to [File] instance and operations,
// allowing users to access file metadata (name, size) for logging/retry logic.
// Plain wrapped errors are used for global archive issues.
// Example:
//
//	_, err := archive.AddFile("data/report.pdf")
//	if err != nil {
//	    var fileErr *gozip.FileError
//	    if errors.As(err, &fileErr) {
//	        fmt.Printf("Operation: %s\n", fileErr.Op)   // e.g., "add", "compress", "extract"
//	        fmt.Printf("File:      %s\n", fileErr.File.Name())
//	        fmt.Printf("Cause:     %v\n", fileErr.Err)  // Underlying error (e.g., [ErrPasswordMismatch])
//	    }
//	}
//
// # Manual Control (Low-Level)
//
// Creating an archive:
//
//	archive := gozip.NewZip()
//	archive.AddFile("file.txt")
//	archive.AddDir("images/", WithCompression(gozip.Deflate, gozip.DeflateMaximum))
//
//	f, _ := os.Create("output.zip")
//	archive.WriteTo(f)
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
//	files, _ := archive.Remove("data/config.json")
//	archive.AddLazy(files[0].Name(), func() (io.ReadCloser, error) {
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
//	archive.WriteTo(dest, gozip.WithWorkers(runtime.NumCPU()))
//
//	// Close source after the work is done
//	src.Close()
package gozip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SizeUnknown is a sentinel value used when the data size cannot
// be determined before writing (e.g., streaming from [io.Reader]).
const SizeUnknown int64 = -1

// ZipConfig defines global configuration parameters for the
// archive. These settings apply to the entire archive but
// can be overridden per-file using [FileConfig] options.
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

	// FileSortStrategy determines the order of file
	// processing and their order in the written archive.
	FileSortStrategy FileSortStrategy

	// ConflictHandler defines the strategy for handling
	// duplicate file names during [Zip.Load]. If nil,
	// defaults to [ActionReplace] (Last Write Wins).
	ConflictHandler ConflictHandler

	// TextDecoder handles filename decoding for legacy archives (non-UTF8).
	// This function is only used in read operations. GoZip always sets
	// the UTF-8 flag for maximum compatibility when writing.
	// Default: [DecodeCP437] (IBM PC).
	TextDecoder TextDecoder

	// OnFileDone is a callback triggered after a file is written, read, or extracted.
	// Errors are not wrapped in [FileError], because file instance is passed separately.
	// This callback can be used to stop bulk operations on the first error.
	//
	// WARNING: This callback may be triggered concurrently if [WithWorkers] is used.
	OnFileDone func(*File, error)

	// MemoryThreshold determines the maximum file size in bytes that
	// can be buffered in memory. If the file size exceeds this threshold,
	// a temporary file will be used. The default value is 10 MB.
	MemoryThreshold int64

	// IncludeImplicitDirs determines whether to include implicitly
	// created dirs in the resulting archive for saving specific metadata.
	IncludeImplicitDirs bool
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

// Zip represents an in-memory ZIP archive manager. It is concurrency-safe
// and supports streaming, random access, and parallel operations.
//
// By default it supports [Store] (no compression) and [Deflate] compression methods.
type Zip struct {
	mu            sync.RWMutex     // Guards files, lookup, and config
	config        ZipConfig        // Global settings
	files         []*File          // List of parsed entries
	lookup        map[string]*File // Lookup map for existence checks (normalized paths)
	factories     factoriesMap     // Factories for creating new compressors (Method -> Factory)
	decompressors decompressorsMap // Registered decompression codecs
	bufferPool    sync.Pool        // Pool of 64KB buffers for IO optimization
}

// NewZip creates a ready-to-use empty ZIP archive.
// Default support includes [Store] (No Compression) and [Deflate].
func NewZip(opts ...ArchiveOption) *Zip {
	z := &Zip{
		files:         make([]*File, 0),
		lookup:        make(map[string]*File),
		factories:     make(factoriesMap),
		decompressors: make(decompressorsMap),
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 64*1024) // 64KB
			},
		},
	}
	z.registerDefaults()

	for _, opt := range opts {
		opt(z)
	}

	return z
}

// Config returns current global zip configuration.
func (z *Zip) Config() ZipConfig {
	z.mu.RLock()
	defer z.mu.RUnlock()
	return z.config
}

// SetConfig updates the global configuration atomically.
// The current password is applied to all loaded files.
func (z *Zip) SetConfig(c ZipConfig) *Zip {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.config = c
	return z
}

// RegisterCompressor registers a factory function for a specific compression method.
// See [NewDeflateCompressor] for creating custom compressors.
func (z *Zip) RegisterCompressor(method CompressionMethod, factory CompressorFactory) *Zip {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.factories[method] = factory
	return z
}

// RegisterDecompressor adds support for reading a custom compression method.
// See [DeflateDecompressor] for creating custom decompressors.
func (z *Zip) RegisterDecompressor(method CompressionMethod, d Decompressor) *Zip {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.decompressors[method] = d
	return z
}

// FS returns [fs.FS], a read-only virtual filesystem on top of the ZIP archive.
func (z *Zip) FS() fs.FS {
	return &zipFS{z: z}
}

// Add adds a pre-configured [File] object to the archive.
// Paths are normalized to use forward slashes.
//
// Unlike [Zip.Load], add methods are strict: if a file with
// the same name already exists in the archive, they return
// [ErrDuplicateEntry] and do not overwrite the existing entry.
//
// Options can be used to override compression, encryption, or file attributes.
func (z *Zip) Add(f *File, opts ...AddOption) error {
	if f == nil {
		return wrapErr("add", nil, fmt.Errorf("file cannot be nil"))
	}
	return wrapErr("add", f, z.addEntry(f, opts))
}

// AddFile adds a file from the local filesystem to the archive.
// Symlinks are stored as link targets and are not followed.
func (z *Zip) AddFile(path string, opts ...AddOption) (*File, error) {
	fileEntry, err := newFileFromPath(path)
	if err != nil {
		return nil, wrapErr("add", nil, err)
	}
	return fileEntry, z.Add(fileEntry, opts...)
}

// AddOSFile adds an open [os.File] to the archive.
// The file content is wrapped using [io.SectionReader].
func (z *Zip) AddOSFile(f *os.File, opts ...AddOption) (*File, error) {
	fileEntry, err := newFileFromOS(f)
	if err != nil {
		return nil, wrapErr("add", nil, err)
	}
	return fileEntry, z.Add(fileEntry, opts...)
}

// AddDir recursively adds contents of the directory to the archive.
// Files are added using "Best Effort" strategy: if a single file fails to read,
// AddDir continues processing others but returns a joined error at the end.
func (z *Zip) AddDir(path string, opts ...AddOption) ([]*File, error) {
	var errs []error
	var files []*File

	baseOpts := make([]AddOption, 0, len(opts)+1)
	baseOpts = append(baseOpts, nil)
	baseOpts = append(baseOpts, opts...)

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
			return nil
		}

		baseOpts[0] = WithPath(filepath.ToSlash(filepath.Dir(relPath)))

		f, err := z.AddFile(walkPath, baseOpts...)
		if err != nil {
			errs = append(errs, err)
		} else {
			files = append(files, f)
		}

		return nil
	})

	if walkErr != nil {
		errs = append(errs, wrapErr("add", nil, walkErr))
	}

	return files, errors.Join(errs...)
}

// AddFS adds files from an [fs.FS] (e.g., [embed.FS], [os.DirFS]) to the archive.
// It recursively walks the file system and adds all entries using "Best Effort" strategy.
func (z *Zip) AddFS(fileSystem fs.FS, opts ...AddOption) ([]*File, error) {
	var errs []error
	var files []*File
	seen := make(map[string]bool)

	walkErr := fs.WalkDir(fileSystem, ".", func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, wrapErr("add", nil, fmt.Errorf("scan %s: %w", walkPath, err)))
			return nil
		}

		cleanPath := path.Clean(walkPath)

		if cleanPath == "." || seen[cleanPath] {
			return nil
		}
		seen[cleanPath] = true

		info, err := d.Info()
		if err != nil {
			errs = append(errs, wrapErr("add", nil, fmt.Errorf("stat %s: %w", walkPath, err)))
			return nil
		}

		f, err := newFileFromFS(fileSystem, cleanPath, info)
		if err != nil {
			errs = append(errs, wrapErr("create", f, err))
			return nil
		}

		if err = z.Add(f); err != nil {
			errs = append(errs, err)
		} else {
			files = append(files, f)
		}

		return nil
	})

	if walkErr != nil {
		errs = append(errs, wrapErr("add", nil, walkErr))
	}

	return files, errors.Join(errs...)
}

// AddReader adds a file from an [io.Reader] stream.
//
// If size is [SizeUnknown] and the target writer is an [io.Seeker] (e.g., os.File),
// the writer will buffer the entire stream to a temporary file to calculate
// headers before writing. To avoid this, provide the exact size if possible.
//
// Returns [ErrFileEntry] if an invalid argument is passed.
func (z *Zip) AddReader(r io.Reader, filename string, size int64, opts ...AddOption) (*File, error) {
	fileEntry, err := newFileFromReader(r, filename, size)
	if err != nil {
		return nil, wrapErr("add", nil, err)
	}
	return fileEntry, z.Add(fileEntry, opts...)
}

// AddLazy adds a file entry whose content is opened only when writing the archive.
//
// The [io.ReadCloser] returned by openFunc is automatically closed by the library
// after the file is written. You do not need to wrap it to close it manually,
// but you are responsible for closing any resources used to create that reader
// (e.g. database connections) inside the closure or after [Zip.WriteTo] finishes.
//
// Returns [ErrFileEntry] if an invalid name is passed.
func (z *Zip) AddLazy(name string, openFunc func() (io.ReadCloser, error), opts ...AddOption) (*File, error) {
	fileEntry, err := newFileFromReader(io.LimitReader(nil, 0), name, SizeUnknown)
	if err != nil {
		return nil, wrapErr("add", nil, err)
	}
	fileEntry.openFunc = openFunc
	return fileEntry, z.Add(fileEntry, opts...)
}

// AddBytes adds a file from a byte slice.
// Returns [ErrFileEntry] if an invalid argument is passed.
func (z *Zip) AddBytes(data []byte, filename string, opts ...AddOption) (*File, error) {
	return z.AddReader(bytes.NewReader(data), filename, int64(len(data)), opts...)
}

// AddString adds a file from a string.
// Returns [ErrFileEntry] if an invalid argument is passed.
func (z *Zip) AddString(content string, filename string, opts ...AddOption) (*File, error) {
	return z.AddReader(strings.NewReader(content), filename, int64(len(content)), opts...)
}

// Mkdir creates an explicit directory entry in the archive.
// Returns [ErrFileEntry] if invalid name is passed.
func (z *Zip) Mkdir(name string, opts ...AddOption) (*File, error) {
	dirEntry, err := newDirectoryFile(name)
	if err != nil {
		return nil, wrapErr("add", nil, err)
	}
	return dirEntry, z.Add(dirEntry, opts...)
}

// Remove deletes an entry from the archive. If the target is a directory,
// it recursively removes all its contents and the directory entry itself.
//
// Providing an empty string or "." results in a "Reset" operation:
// all entries are removed, and the archive becomes empty.
//
// Returns [ErrFileNotFound] if no entries matched the provided name.
func (z *Zip) Remove(name string) ([]*File, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	if name == "" || name == "." {
		if len(z.files) == 0 {
			return nil, nil
		}
		files := make([]*File, len(z.files))
		copy(files, z.files)
		z.files = z.files[:0]
		clear(z.lookup)
		return files, nil
	}

	cleanName := z.normalizePath(name)
	dirPrefix := cleanName + "/"

	var deleted []*File
	var n int
	for _, file := range z.files {
		fName := file.entryName()

		isExactMatch := fName == cleanName || fName == dirPrefix
		isChild := strings.HasPrefix(fName, dirPrefix)

		if isExactMatch || isChild {
			deleted = append(deleted, file)
			delete(z.lookup, fName)
			continue
		}

		z.files[n] = file
		n++
	}

	if len(deleted) == 0 {
		return nil, wrapErr("remove", nil, fmt.Errorf("%w: '%s'", ErrFileNotFound, name))
	}

	for i := n; i < len(z.files); i++ {
		z.files[i] = nil
	}

	z.files = z.files[:n]

	return deleted, nil
}

// Rename changes the name of an entry while preserving its current
// directory location. If target is a directory, it recursively renames
// all nested files and subdirectories to reflect the new parent name.
// Example: Rename("logs/old.txt", "new.txt") -> "logs/new.txt"
//
// This operation is atomic. It performs a "dry run" check of all
// resulting paths. If any path (including children) exceeds ZIP limits
// or conflicts with existing entries, no changes are applied to the archive.
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
// If target is a directory, the entire tree is moved recursively to the new location.
// Any missing parent directories in the destination path are automatically created.
// Example: Move("etc/file.txt", "backup/docs") -> "backup/docs/file.txt".
//
// Like [Zip.Rename], this operation is atomic. It validates all
// resulting child paths before modifying the archive structure.
//
// Errors:
//   - Returns [ErrFileNotFound] if the old entry does not exist.
//   - Returns [ErrDuplicateEntry] if the destination path is already occupied.
//   - Returns [ErrFilenameTooLong] if any resulting path exceeds 65,535 bytes.
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

// File returns the entry matching the given name and wether it exists.
// Name is case-sensitive. Paths are normalized to use forward slashes.
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

// Select returns a list of files that satisfy the given condition.
func (z *Zip) Select(filters ...Filter) []*File {
	z.mu.RLock()
	defer z.mu.RUnlock()

	matches := make([]*File, 0, len(z.files)/2)

	for _, f := range z.files {
		isMatch := true
		for _, filter := range filters {
			if !filter(f) {
				isMatch = false
				break
			}
		}
		if isMatch {
			matches = append(matches, f)
		}
	}

	return matches
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

	matches := make([]*File, 0, len(z.files)/2)

	for _, f := range z.files {
		if matched, _ := path.Match(pattern, f.name); matched {
			matches = append(matches, f)
		}
	}

	return matches, nil
}

// Find searches for files matching the pattern in all directories.
// Unlike Glob, the pattern "*" matches "/" characters.
// Example: Find("*.log") matches "error.log" and "var/logs/access.log".
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

// WriteTo serializes the archive to the specified writer. Files are
// processes using "Best Effort" strategy. If the writer is [io.Seeker],
// temporary files are used if size exceeds [ZipConfig.MemoryThreshold]).
//
// Use [WithWorkers] option to speed the compression significantly. Speed scales efficiently
// with CPU cores for compression-heavy tasks (Deflate/AES). Peak memory usage is strictly
// bounded by the pipeline capacity (maxWorkers * 2) and the MemoryThreshold (default 10MB).
//
// Efficiency Note: Processing files in descending order of size ([SortSizeDescending])
// helps finish long-running compression tasks early and can stabilize memory usage,
// though it may be slightly slower for small archives.
//
// Returns the total number of bytes written or an error if the operation fails.
func (z *Zip) WriteTo(dest io.Writer, opts ...ZipOption) (int64, error) {
	return z.WriteToWithContext(context.Background(), dest, opts...)
}

// WriteToWithContext writes the archive with context support.
// Cancelling the context stops processing the remaining files and results in a valid archive.
func (z *Zip) WriteToWithContext(ctx context.Context, dest io.Writer, opts ...ZipOption) (int64, error) {
	cfg := z.applyOptions(opts)
	files := z.Select(cfg.filters...)
	files = SortFilesOptimized(files, z.config.FileSortStrategy)

	if cfg.password != "" {
		for _, f := range files {
			f.WithPassword(cfg.password)
		}
	}

	tracker := &atomicCounterWriter{w: dest}
	var writerDest io.Writer = tracker
	if seeker, ok := dest.(io.WriteSeeker); ok {
		writerDest = &atomicCounterWriteSeeker{atomicCounterWriter: tracker, seeker: seeker}
	}

	collector := newStatsCollector(cfg, files, tracker)

	writer := newZipWriter(z.config, z.factories, writerDest)
	writer.onRead = collector.OnRead
	writer.onCompressed = collector.OnWritten

	var errs []error
	if workers := z.getWorkers(cfg, files); workers > 1 {
		errs = z.execParallelWrite(ctx, files, writer, workers, collector.OnFileDone)
	} else {
		errs = z.execSequentialWrite(ctx, files, writer, collector.OnFileDone)
	}

	if err := writer.WriteCentralDirAndEndRecords(); err != nil {
		errs = append(errs, fmt.Errorf("zip: finalize: %w", err))
	}

	collector.Finish()

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return tracker.Count(), errors.Join(errs...)
}

// WriteHTTP writes the archive to the HTTP response writer with correct headers.
//
// If an error occurs before writing data (e.g. empty archive), it does not write
// headers, allowing the caller to send an HTTP 500/400.
//
// If an error occurs during writing, the download will be truncated/corrupted
// (which is the only way to signal failure to the client after headers are sent).
// In this case, the error is returned for server-side logging.
func (z *Zip) WriteHTTP(w http.ResponseWriter, filename string, opts ...ZipOption) error {
	return z.WriteHTTPWithContext(context.Background(), w, filename, opts...)
}

func (z *Zip) WriteHTTPWithContext(ctx context.Context, w http.ResponseWriter, filename string, opts ...ZipOption) error {
	filename = filepath.Base(filename)
	if filename == "" || filename == "." {
		filename = "archive.zip"
	}

	header := w.Header()

	header.Set("Content-Type", "application/zip")
	header.Set("X-Content-Type-Options", "nosniff") // Prevent browser from guessing content type

	// Prevent caching for generated content
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")

	// mime.FormatMediaType automatically handles special characters and UTF-8
	// creating: attachment; filename="name.zip"; filename*=UTF-8''name%20.zip
	disposition := mime.FormatMediaType("attachment", map[string]string{
		"filename": filename,
	})
	header.Set("Content-Disposition", disposition)

	// Note: We use the ResponseWriter directly.
	// If the client disconnects, w.Write() will return an error (Broken Pipe),
	// which will stop the compression process.
	_, err := z.WriteToWithContext(ctx, w, opts...)
	return err
}

// Load parses an existing ZIP archive's central directory and merges its entries
// into the current Zip instance using "Best Effort" strategy. It supports standard
// ZIP, Zip64, and archives with preambles (e.g., self-extracting EXEs).
//
// If the current archive already contains entries with the same name as in
// the source, the existing entries are replaced by the new ones by default.
// You can change logic by specifying custom [ZipConfig.ConflictHandler].
//
// Returns [ErrFormat] if the source is not a valid ZIP archive.
func (z *Zip) Load(src io.ReaderAt, size int64) ([]*File, error) {
	return z.LoadWithContext(context.Background(), src, size)
}

// LoadWithContext parses an archive with context support.
// Cancelling the context stops processing the remaining files.
func (z *Zip) LoadWithContext(ctx context.Context, src io.ReaderAt, size int64) ([]*File, error) {
	reader := newZipReader(src, size, z.decompressors, z.config)
	eocd, err := reader.FindAndReadEOCD(ctx)
	if err != nil {
		return nil, err
	}

	if z.config.Comment == "" {
		z.config.Comment = eocd.Comment
	}

	files, err := reader.ReadFiles(ctx, eocd)
	if err != nil {
		return nil, err
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

		if added && z.config.OnFileDone != nil {
			z.config.OnFileDone(file, err)
		}
	}

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return files, errors.Join(errs...)
}

// LoadFromFile parses a ZIP from a local os.File.
func (z *Zip) LoadFromFile(f *os.File) ([]*File, error) {
	return z.LoadFromFileWithContext(context.Background(), f)
}

// LoadFromFile parses a ZIP from a local os.File with context support.
func (z *Zip) LoadFromFileWithContext(ctx context.Context, f *os.File) ([]*File, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return z.LoadWithContext(ctx, f, stat.Size())
}

// Verify checks the integrity of the archive files. It decompresses
// every file and verifies checksums/MACs without writing to disk.
func (z *Zip) Verify(opts ...ZipOption) error {
	return z.VerifyWithContext(context.Background(), opts...)
}

// VerifyWithContext checks integrity with context cancellation.
func (z *Zip) VerifyWithContext(ctx context.Context, opts ...ZipOption) error {
	cfg := z.applyOptions(opts)
	files := z.Select(cfg.filters...)

	if len(files) == 0 {
		return nil
	}

	if cfg.password != "" {
		for _, f := range files {
			f.WithPassword(cfg.password)
		}
	}

	collector := newStatsCollector(cfg, files, nil)

	var errs []error
	if workers := z.getWorkers(cfg, files); workers > 0 {
		errs = z.execParallelVerify(ctx, files, workers, collector)
	} else {
		errs = z.execSequentialVerify(ctx, files, collector)
	}

	collector.Finish()

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ExtractTo unpacks the archive to the specified destination directory using "Best Effort" strategy.
// It automatically creates missing directory structures and restores file modification times and permissions.
// Attempts to extract files outside the target directory will result in [ErrInsecurePath].
func (z *Zip) ExtractTo(path string, opts ...ZipOption) error {
	return z.ExtractToWithContext(context.Background(), path, opts...)
}

// ExtractToWithContext extracts files with context support.
// Context cancellation stops the extraction process.
func (z *Zip) ExtractToWithContext(ctx context.Context, path string, opts ...ZipOption) error {
	path = filepath.Clean(path)
	cfg := z.applyOptions(opts)
	files := z.Select(cfg.filters...)
	sortAlphabetical(files)

	collector := newStatsCollector(cfg, files, nil)

	if cfg.password != "" {
		for _, f := range files {
			f.WithSourcePassword(cfg.password)
		}
	}

	var errs []error
	var dirsToRestore []*File
	if workers := z.getWorkers(cfg, files); workers > 1 {
		dirsToRestore, errs = z.execParallelExtract(ctx, files, path, workers, collector, cfg)
	} else {
		dirsToRestore, errs = z.execSequentialExtract(ctx, files, path, collector, cfg)
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

// Internal strategy executors

func (z *Zip) execSequentialWrite(ctx context.Context, files []*File, writer *zipWriter, onFileDone func(*File, error)) []error {
	var errs []error
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		err := writer.WriteFile(file)
		if err != nil {
			errs = append(errs, wrapErr("write", file, err))
		}
		onFileDone(file, err)
	}
	return errs
}

func (z *Zip) execParallelWrite(ctx context.Context, files []*File, writer *zipWriter, workers int, onFileDone func(*File, error)) []error {
	pzw := newParallelZipWriter(writer, workers)
	pzw.onFileDone = onFileDone
	return pzw.WriteFiles(ctx, files)
}

func (z *Zip) execSequentialVerify(ctx context.Context, files []*File, collector *statsCollector) []error {
	var errs []error
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		err := z.verifySingleFile(f, collector.OnRead)
		if err != nil {
			errs = append(errs, wrapErr("verify", f, err))
		}
		collector.OnFileDone(f, err)
	}
	return errs
}

func (z *Zip) execParallelVerify(ctx context.Context, files []*File, workers int, collector *statsCollector) []error {
	tasks := make(chan *File)
	errChan := make(chan error, len(files))
	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for f := range tasks {
				if ctx.Err() != nil {
					return
				}
				err := z.verifySingleFile(f, collector.OnRead)
				if err != nil {
					errChan <- wrapErr("verify", f, err)
				}
				collector.OnFileDone(f, err)
			}
		})
	}

	func() {
		for _, f := range files {
			select {
			case <-ctx.Done():
				return
			case tasks <- f:
			}
		}
	}()

	close(tasks)
	wg.Wait()
	close(errChan)

	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}
	return errs
}

func (z *Zip) execSequentialExtract(
	ctx context.Context, files []*File, destDir string, collector *statsCollector, cfg processConfig,
) ([]*File, []error) {
	var errs []error
	var globalWritten int64
	dirsToRestore := make([]*File, 0, len(files)/2)

	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		if f.config.EncryptionMethod < cfg.security.MinEncryption {
			err := fmt.Errorf("%w: encryption method too weak (required %v, got %v)",
				ErrInsecurePath, cfg.security.MinEncryption, f.config.EncryptionMethod)
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		limits := cfg.security.ResourceLimits
		if limits.MaxFileSize > 0 && f.UncompressedSize() > limits.MaxFileSize {
			errs = append(errs, wrapErr("extract", f, ErrResourceLimit))
			continue
		}

		fpath, err := SafePath(destDir, f.name)
		if err != nil {
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		if f.isDir {
			err := os.MkdirAll(fpath, 0755)
			if err != nil {
				errs = append(errs, wrapErr("extract", f, err))
			} else {
				dirsToRestore = append(dirsToRestore, f)
			}
			collector.OnFileDone(f, err)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		err = z.extractFile(ctx, f, destDir, f.name, collector.OnRead, collector.OnWritten, &globalWritten, cfg)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			errs = append(errs, wrapErr("extract", f, err))
		}
		collector.OnFileDone(f, err)
	}

	return dirsToRestore, errs
}

func (z *Zip) execParallelExtract(
	ctx context.Context, files []*File, destDir string, workers int, collector *statsCollector, cfg processConfig,
) ([]*File, []error) {
	var globalWritten int64
	filesToExtract := make([]*File, 0, len(files))
	dirsToRestore := make([]*File, 0, len(files)/2)

	var errs []error
	for _, f := range files {
		if f.config.EncryptionMethod < cfg.security.MinEncryption {
			err := fmt.Errorf("%w: encryption method too weak (required %v, got %v)",
				ErrInsecurePath, cfg.security.MinEncryption, f.config.EncryptionMethod)
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		fpath, err := SafePath(destDir, f.name)
		if err != nil {
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		if err := checkSymlinkTraversal(fpath, destDir); err != nil {
			errs = append(errs, wrapErr("extract", f, err))
			collector.OnFileDone(f, err)
			continue
		}

		if f.isDir {
			err := os.MkdirAll(fpath, 0755)
			if err != nil {
				errs = append(errs, wrapErr("extract", f, err))
			} else {
				dirsToRestore = append(dirsToRestore, f)
			}
			collector.OnFileDone(f, err)
			continue
		}

		filesToExtract = append(filesToExtract, f)
	}

	if len(errs) > 0 {
		return dirsToRestore, errs
	}

	tasks := make(chan *File, workers*2)
	errChan := make(chan error, len(filesToExtract))
	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for f := range tasks {
				if ctx.Err() != nil {
					return
				}
				err := z.extractFile(ctx, f, destDir, f.name, collector.OnRead, collector.OnWritten, &globalWritten, cfg)
				if err != nil {
					if ctx.Err() == nil {
						errChan <- wrapErr("extract", f, err)
					}
				}
				collector.OnFileDone(f, err)
			}

		})
	}

	func() {
		for _, f := range filesToExtract {
			select {
			case <-ctx.Done():
				return
			case tasks <- f:
			}
		}
	}()

	close(tasks)
	wg.Wait()
	close(errChan)

	for err := range errChan {
		errs = append(errs, err)
	}

	return dirsToRestore, errs
}

// Internal helpers

func (z *Zip) registerDefaults() {
	z.RegisterCompressor(Store, newStoreCompressor)
	z.RegisterDecompressor(Store, new(storeDecompressor))
	z.RegisterCompressor(Deflate, NewDeflateCompressor)
	z.RegisterDecompressor(Deflate, new(DeflateDecompressor))
}

// addEntry validates and adds a file to the archive.
// It normalizes paths, checks for duplicates, and ensures parent directories exist.
// Returns an error if the file name is invalid or conflicts with an existing entry.
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

	if f.name == "" || f.name == "." {
		return fmt.Errorf("%w: invalid filename", ErrFileEntry)
	}
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
	for i := range len(name) {
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

func (z *Zip) verifySingleFile(f *File, onRead signalFunc) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	var src io.Reader = rc
	if onRead != nil {
		src = newProgressReader(rc, f, onRead)
	}

	buf := z.bufferPool.Get().([]byte)
	_, err = io.CopyBuffer(io.Discard, src, buf)
	z.bufferPool.Put(buf)

	if err != nil {
		return err
	}

	err = rc.Close()
	if errors.Is(err, ErrChecksum) && f.srcConfig.EncryptionMethod == AES256 {
		// AES256 doesn't store file CRC, because it uses the MAC for integrity
		return nil
	}
	return err
}

// extractFile handles low-level extraction logic.
// It uses the shared buffer pool and attempts to restore file metadata (times/perms).
func (z *Zip) extractFile(
	ctx context.Context, f *File, destDir, fileName string, onRead, onWrite signalFunc, globalWritten *int64, cfg processConfig,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if !cfg.security.AllowSymlinks && f.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlinks are disabled", ErrInsecurePath)
	}

	fpath, _ := SafePath(destDir, fileName)

	if cfg.security.AllowSymlinks {
		if err := checkSymlinkTraversal(fpath, destDir); err != nil {
			return err
		}
	}

	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dest, err := os.Create(fpath)
	if err != nil {
		return err
	}
	defer dest.Close()

	var r io.Reader = src

	if f.UncompressedSize() > 0 {
		r = io.LimitReader(src, f.UncompressedSize())
	}

	limits := cfg.security.ResourceLimits
	if limits.MaxFileSize > 0 && f.UncompressedSize() > limits.MaxFileSize {
		return fmt.Errorf("%w: file header claims %d bytes, limit is %d", ErrResourceLimit, f.UncompressedSize(), limits.MaxFileSize)
	}

	var wrappedR io.Reader = r

	if onRead != nil {
		wrappedR = newProgressReader(r, f, onRead)
	}

	var w io.Writer = dest

	hasLimits := limits.MaxTotalSize > 0 || limits.MaxFileSize > 0 || limits.MaxRatio > 0
	if hasLimits {
		cSize := f.CompressedSize()
		if cSize == -1 {
			cSize = 0
		}

		w = &secureWriter{
			w:              dest,
			totalWritten:   globalWritten,
			maxFileSize:    limits.MaxFileSize,
			maxTotalSize:   limits.MaxTotalSize,
			maxRatio:       limits.MaxRatio,
			compressedSize: cSize,
		}
	}

	var wrappedW io.Writer = w

	if onWrite != nil {
		wrappedW = newProgressWriter(w, f, onWrite)
	}

	if f.uncompressedSize > 0 {
		if err := dest.Truncate(f.uncompressedSize); err != nil {
			return err
		}

		buf := z.bufferPool.Get().([]byte)
		_, err = io.CopyBuffer(wrappedW, &contextReader{ctx, wrappedR}, buf)
		z.bufferPool.Put(buf)

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
	_ = os.Chmod(fileName, perm)
	_ = os.Chtimes(fileName, time.Now(), f.modTime)

	return nil
}

// checkSymlinkTraversal verifies that the destination path does not write *through* a symlink.
// This is expensive (requires Lstat), so it's part of the security check.
func checkSymlinkTraversal(destPath, rootDir string) error {
	current := destPath
	rootDir = filepath.Clean(rootDir)

	for current != rootDir && current != "." && current != "/" {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: path component '%s' is a symlink", ErrInsecurePath, current)
			}
		} else if !os.IsNotExist(err) {
			return err
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func (z *Zip) applyOptions(opts []ZipOption) processConfig {
	cfg := processConfig{}
	if cfg.onFileDone != nil {
		cfg.onFileDone = z.config.OnFileDone
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (z *Zip) getWorkers(cfg processConfig, files []*File) int {
	workers := cfg.workers
	if workers <= 0 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}
	return workers
}
