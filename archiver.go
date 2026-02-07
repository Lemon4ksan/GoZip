package gozip

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Source provides an interface for reading a ZIP archive.
// Implementations must support multiple reads (for example, via io.ReaderAt).
type Source interface {
	Open() (io.ReaderAt, int64, error)
	Close() error
}

// Sink provides an interface for writing a ZIP archive.
// Implementations must support file creation and closing.
type Sink interface {
	Create() (io.Writer, error)
	Close() error
}

// FromFile creates a Source from an opened *os.File file.
// The file is not closed after the operation.
func FromFile(f *os.File) Source { return &fileManager{f: f} }

// FromFilePath creates a Source from a file at the specified path.
func FromFilePath(path string) Source { return &fileManager{path: path} }

// FromReader creates a Source from an io.ReaderAt with a known size.
func FromReader(r io.ReaderAt, size int64) Source { return readerManager{r, size} }

// FromURL creates a Source for reading a ZIP archive over HTTP.
// Requirements: the server must support the "Range" header (Accept-Ranges: bytes).
// It uses HTTP Range Requests to read only the necessary parts of the archive.
func FromURL(url string, client *http.Client) Source {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpSource{url: url, client: client}
}

// ToFile creates a Sink for writing to an open *os.File file.
// The file is not closed after the operation.
func ToFile(f *os.File) Sink { return &fileManager{f: f} }

// ToFilePath creates a Sink for writing to a file at the specified path.
func ToFilePath(path string) Sink { return &fileManager{path: path} }

// ToWriter creates a Sink for writing to an io.Writer.
func ToWriter(w io.Writer) Sink { return writerManager{w} }

// Archiver provides a configurable environment for working with ZIP archives.
// By default, it supports the Store (no compression) and Deflate compression methods.
type Archiver struct {
	engineOptions []ArchiveOption
}

// NewArchiver creates a new Archiver instance with the specified options.
func NewArchiver(opts ...ArchiveOption) *Archiver {
	return &Archiver{engineOptions: opts}
}

// newZip creates a new Zip instance with the specified options applied.
func (a *Archiver) newZip(opts ...ArchiveOption) *Zip {
	return NewZip(append(a.engineOptions, opts...)...)
}

// ReadFile reads the contents of a file from the archive into a byte slice.
// Warning: this loads the entire file into memory. For large files, use Extract or StreamReader.
func (a *Archiver) ReadFile(zip Source, filename string) ([]byte, error) {
	return a.ReadFileWithContext(context.Background(), zip, filename)
}

// ReadFileWithContext reads file content with context support.
// Cancelling the context stops the reading process.
func (a *Archiver) ReadFileWithContext(ctx context.Context, zip Source, filename string) (data []byte, err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	rc, err := a.openFile(filename, archive)
	if err != nil {
		return
	}
	defer func() {
		closeErr := rc.Close()
		if err == nil {
			err = closeErr
		}
	}()

	data, err = io.ReadAll(rc)
	return
}

// ReplaceFile replaces a file in the archive with new content.
// The other files are copied without changes.
func (a *Archiver) ReplaceFile(src Source, dest Sink, filename string, content io.Reader) error {
	return a.ReplaceFileWithContext(context.Background(), src, dest, filename, content)
}

// ReplaceFileWithContext replaces a file in the archive with new content with context support.
func (a *Archiver) ReplaceFileWithContext(ctx context.Context, src Source, dest Sink, filename string, content io.Reader) error {
	return a.TransformWithContext(ctx, src, dest, func(f *File) (io.Reader, error) {
		if f.Name() == filename {
			// Replace content. Note: SizeUnknown implies buffering if Sink is not seekable.
			// But since we are inside Transform, the file header is rewritten anyway.
			return content, nil
		}
		// Keep original content
		return nil, nil
	})
}

// GetEntries returns a list of files in the archive without loading their contents.
// This is a fast operation: it only reads the Central Directory.
func (a *Archiver) GetEntries(zip Source) ([]*File, error) {
	return a.GetEntriesWithContext(context.Background(), zip)
}

// GetEntriesWithContext lists files with context support.
func (a *Archiver) GetEntriesWithContext(ctx context.Context, zip Source) (files []*File, err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return nil, err
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	return archive.Files(), err
}

// Verify checks the integrity of the files in the archive (CRC, checksums).
func (a *Archiver) Verify(zip Source, opts ...ZipOption) error {
	return a.VerifyWithContext(context.Background(), zip, opts...)
}

// Verify checks the integrity of the files in the archive with context support.
func (a *Archiver) VerifyWithContext(ctx context.Context, zip Source, opts ...ZipOption) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	err = archive.VerifyWithContext(ctx, opts...)
	return
}

// Exists checks for the presence of a file in the archive.
func (a *Archiver) Exists(zip Source, filename string) (bool, error) {
	return a.ExistsWithContext(context.Background(), zip, filename)
}

// ExistsWithContext checks if a file exists with context support.
func (a *Archiver) ExistsWithContext(ctx context.Context, zip Source, filename string) (exists bool, err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	exists = archive.Exists(filename)
	return
}

// Walk traverses all files in the archive, calling walkFn for each one.
// If walkFn returns an error, the traversal stops.
func (a *Archiver) Walk(zip Source, walkFn func(*File) error, filters ...Filter) error {
	return a.WalkWithContext(context.Background(), zip, walkFn, filters...)
}

// WalkWithContext iterates over the archive with context cancellation support.
func (a *Archiver) WalkWithContext(ctx context.Context, zip Source, walkFn func(*File) error, filters ...Filter) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	for _, f := range a.applyFilters(archive.Files(), filters) {
		if err = ctx.Err(); err != nil {
			return
		}
		if err = walkFn(f); err != nil {
			return
		}
	}
	return
}

// Search searches for text within the contents of all files in the archive.
// It returns a list of files containing the text.
// Warning: this is a resource-intensive operation, as it unpacks each file.
func (a *Archiver) Search(zip Source, text string, filters ...Filter) ([]*File, error) {
	return a.SearchWithContext(context.Background(), zip, text, filters...)
}

// SearchWithContext scans content with context support.
func (a *Archiver) SearchWithContext(ctx context.Context, zip Source, text string, filters ...Filter) ([]*File, error) {
	var matches []*File

	target := []byte(text)
	if len(target) == 0 {
		return nil, nil
	}

	err := a.WalkWithContext(ctx, zip, func(f *File) error {
		if f.IsDir() {
			return nil
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		const bufSize = 32 * 1024
		buf := make([]byte, bufSize)

		overlap := make([]byte, len(target)-1)
		var hasOverlap bool

		for {
			if err := ctx.Err(); err != nil {
				return err
			}

			n, readErr := rc.Read(buf)
			if n > 0 {
				if hasOverlap {
					combined := append(overlap, buf[:min(len(target), n)]...)
					if bytes.Contains(combined, target) {
						matches = append(matches, f)
						return nil
					}
				}

				if bytes.Contains(buf[:n], target) {
					matches = append(matches, f)
					return nil
				}

				if n >= len(overlap) {
					copy(overlap, buf[n-len(overlap):n])
					hasOverlap = true
				}
			}

			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				return readErr
			}
		}
		return nil
	}, filters...)

	return matches, err
}

// UpdateMetadata modifies the metadata of files in the archive.
// Example: normalizing access permissions, adding comments.
func (a *Archiver) UpdateMetadata(src Source, dest Sink, modifier func(*File), opts ...ZipOption) error {
	return a.UpdateMetadataWithContext(context.Background(), src, dest, modifier, opts...)
}

// UpdateMetadataWithContext allows bulk modification with context support.
func (a *Archiver) UpdateMetadataWithContext(ctx context.Context, src Source, dest Sink, modifier func(*File), opts ...ZipOption) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, src, archive); err != nil {
		return
	}
	defer func() {
		closeErr := src.Close()
		if err == nil {
			err = closeErr
		}
	}()

	for _, f := range archive.Files() {
		if err = ctx.Err(); err != nil {
			return
		}
		modifier(f)
	}

	w, err := dest.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := dest.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = archive.WriteToWithContext(ctx, w, opts...)
	return
}

// TransformFunc defines the logic for modifying a file:
//   - (newReader, nil): replace the content;
//   - (nil, nil): leave unchanged;
//   - (nil, ErrSkip): delete the file.
type TransformFunc func(f *File) (io.Reader, error)

// Transform applies a function to each file in the archive, allowing modification of content.
// Example: Replace all occurrences of "old" with "new" in text files:
//
//	err := archive.Transform(src, dest, func(f *File) (io.Reader, error) {
//	    if strings.HasSuffix(f.Name(), ".txt") {
//	        return strings.NewReader(strings.ReplaceAll(f.Content(), "old", "new")), nil
//	    }
//	    return nil, nil // Keep original content
//	})
func (a *Archiver) Transform(src Source, dest Sink, fn TransformFunc, opts ...ZipOption) error {
	return a.TransformWithContext(context.Background(), src, dest, fn, opts...)
}

// TransformWithContext applies a function to each file in the archive, allowing modification of content with context support.
func (a *Archiver) TransformWithContext(ctx context.Context, src Source, dest Sink, fn TransformFunc, opts ...ZipOption) (err error) {
	input := a.newZip()
	if err = a.loadZip(ctx, src, input); err != nil {
		return
	}
	defer func() {
		closeErr := src.Close()
		if err == nil {
			err = closeErr
		}
	}()

	output := a.newZip().SetConfig(input.Config())

	for _, file := range input.Files() {
		if err := ctx.Err(); err != nil {
			return err
		}

		newContent, err := fn(file)
		if err != nil {
			return err
		}

		if newContent != nil {
			// Add modified content (size unknown, will be buffered or streamed with descriptor)
			output.AddReader(newContent, file.Name(), SizeUnknown)
		} else {
			// Add original file (Zero-copy optimization if possible)
			// We need to re-add it to the new struct
			output.files = append(output.files, file)
			output.lookup[file.Name()] = file
		}
	}

	w, err := dest.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := dest.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = output.WriteToWithContext(ctx, w, opts...)
	return
}

// ArchiveDiff describes the differences between two archives.
type ArchiveDiff struct {
	Added    []string // Files present only in B.
	Removed  []string // Files present only in A.
	Modified []string // Files with differing CRCs or sizes.
}

// Diff compares two archives and returns the differences.
func (a *Archiver) Diff(srcA, srcB Source, filters ...Filter) (ArchiveDiff, error) {
	return a.DiffWithContext(context.Background(), srcA, srcB, filters...)
}

func (a *Archiver) DiffWithContext(ctx context.Context, srcA, srcB Source, filters ...Filter) (diff ArchiveDiff, err error) {
	zipA := a.newZip()
	zipB := a.newZip()

	if err = a.loadZip(ctx, srcA, zipA); err != nil {
		return
	}
	defer func() {
		closeErr := srcA.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if err = a.loadZip(ctx, srcB, zipB); err != nil {
		return
	}
	defer func() {
		closeErr := srcB.Close()
		if err == nil {
			err = closeErr
		}
	}()

	mapA := make(map[string]*File)
	for _, f := range a.applyFilters(zipA.Files(), filters) {
		mapA[f.Name()] = f
	}

	mapB := make(map[string]*File)
	for _, f := range a.applyFilters(zipB.Files(), filters) {
		mapB[f.Name()] = f
	}

	// Check for Removed and Modified
	for name, fileA := range mapA {
		if fileB, ok := mapB[name]; !ok {
			diff.Removed = append(diff.Removed, name)
		} else {
			// Simple check via CRC32.
			// Note: If one file is encrypted (AES) and other isn't, CRC might be 0.
			if fileA.CRC32() != fileB.CRC32() && fileA.CRC32() != 0 && fileB.CRC32() != 0 {
				diff.Modified = append(diff.Modified, name)
			} else if fileA.UncompressedSize() != fileB.UncompressedSize() {
				// Fallback to size check if CRC is unavailable/zero
				diff.Modified = append(diff.Modified, name)
			}
		}
	}

	// Check for Added
	for name := range mapB {
		if _, ok := mapA[name]; !ok {
			diff.Added = append(diff.Added, name)
		}
	}

	return
}

// Tree returns a text representation of the archive structure (similar to the `tree` command).
func (a *Archiver) Tree(src Source, filters ...Filter) (string, error) {
	return a.TreeWithContext(context.Background(), src, filters...)
}

// Tree returns a string representation of the archive structure with context support.
func (a *Archiver) TreeWithContext(ctx context.Context, src Source, filters ...Filter) (string, error) {
	files, err := a.GetEntriesWithContext(ctx, src)
	if err != nil {
		return "", err
	}
	for _, filter := range filters {
		files = filter(files)
	}
	return generateTree(files), nil
}

// TotalSize returns the total size of compressed and uncompressed data in the archive.
func (a *Archiver) TotalSize(zip Source, filters ...Filter) (uncompressed, compressed int64, err error) {
	files, err := a.GetEntries(zip)
	if err != nil {
		return
	}
	for _, f := range a.applyFilters(files, filters) {
		uncompressed += f.UncompressedSize()
		compressed += f.CompressedSize()
	}
	return
}

// IsEncrypted checks if the archive contains at least one encrypted file.
func (a *Archiver) IsEncrypted(zip Source, filters ...Filter) (bool, error) {
	files, err := a.GetEntries(zip)
	if err != nil {
		return false, err
	}
	for _, f := range a.applyFilters(files, filters) {
		if f.IsEncrypted() {
			return true, nil
		}
	}
	return false, nil
}

// ArchiveDir recursively archives the contents of a directory.
// By default: Deflate compression, relative paths are preserved.
func (a *Archiver) ArchiveDir(srcDir string, destZip Sink, opts ...ZipOption) error {
	return a.ArchiveDirWithContext(context.Background(), srcDir, destZip, opts...)
}

// ArchiveDirWithContext recursively adds contents of the directory with context support.
// Cancelling the context stops processing remaining files and closes the destination file.
func (a *Archiver) ArchiveDirWithContext(ctx context.Context, srcDir string, destZip Sink, opts ...ZipOption) (err error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrFileEntry, srcDir)
	}

	archive := a.newZip()
	if _, err = archive.AddDir(srcDir); err != nil {
		return
	}

	dest, err := destZip.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := destZip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = archive.WriteToWithContext(ctx, dest, opts...)
	return
}

// ArchiveFiles archives a list of files.
// File paths are "flattened" and placed in the root of the archive.
func (a *Archiver) ArchiveFiles(files []string, destZip Sink, opts ...ZipOption) error {
	return a.ArchiveFilesWithContext(context.Background(), files, destZip, opts...)
}

// ArchiveFilesWithContext creates an archive from a list of files with context support.
func (a *Archiver) ArchiveFilesWithContext(ctx context.Context, files []string, destZip Sink, opts ...ZipOption) (err error) {
	archive := a.newZip()
	for _, file := range files {
		if _, err = archive.AddFile(file, WithName(filepath.Base(file))); err != nil {
			return
		}
	}

	dest, err := destZip.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := destZip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = archive.WriteToWithContext(ctx, dest, opts...)
	return
}

// Merge combines multiple archives into one.
// Conflict resolution strategy: "the last written file wins".
func (a *Archiver) Merge(destZip Sink, sources ...Source) error {
	return a.MergeWithContext(context.Background(), destZip, sources...)
}

// MergeWithContext combines archives with context support.
func (a *Archiver) MergeWithContext(ctx context.Context, destZip Sink, sources ...Source) (err error) {
	archive := a.newZip()
	for _, src := range sources {
		if err = a.loadZip(ctx, src, archive); err != nil {
			_ = src.Close() // Close current on error
			return err
		}
		defer func() {
			closeErr := src.Close()
			if err == nil {
				err = closeErr
			}
		}()
	}

	dest, err := destZip.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := destZip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = archive.WriteToWithContext(ctx, dest)
	return
}

// Clone copies the archive with the possibility of applying filters or modifiers.
// Example: deleting files, changing compression.
func (a *Archiver) Clone(zip Source, destZip Sink, opts ...ZipOption) error {
	return a.CloneWithContext(context.Background(), zip, destZip, opts...)
}

// CloneWithContext copies archive with context support.
func (a *Archiver) CloneWithContext(ctx context.Context, zip Source, destZip Sink, opts ...ZipOption) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	dest, err := destZip.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := destZip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = archive.WriteToWithContext(ctx, dest, opts...)
	return
}

// Unzip extracts the archive to the specified directory.
// Note: It does not support encrypted archives (use UnzipEncrypted).
func (a *Archiver) Unzip(zip Source, destDir string, opts ...ZipOption) error {
	return a.UnzipWithContext(context.Background(), zip, destDir, opts...)
}

// UnzipWithContext extracts archive contents with context support.
// Cancelling the context stops the extraction immediately.
func (a *Archiver) UnzipWithContext(ctx context.Context, zip Source, destDir string, opts ...ZipOption) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	err = archive.ExtractToWithContext(ctx, destDir, opts...)
	return
}

// UnzipFile extracts the specified file from the archive.
func (a *Archiver) UnzipFile(zip Source, filename string, dest Sink) error {
	return a.UnzipFileWithContext(context.Background(), zip, filename, dest)
}

// UnzipFileWithContext extracts the specified file from the archive with context support.
func (a *Archiver) UnzipFileWithContext(ctx context.Context, zip Source, filename string, destPath Sink) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	file, err := a.openFile(filename, archive)
	if err != nil {
		return
	}

	dest, err := destPath.Create()
	if err != nil {
		return
	}
	defer func() {
		closeErr := destPath.Close()
		if err == nil {
			err = closeErr
		}
	}()

	_, err = io.Copy(dest, file)
	return
}

// UnzipToMap extracts all files from the archive into a map[filename]content. Directories are ignored.
func (a *Archiver) UnzipToMap(src Source, filters ...Filter) (map[string][]byte, error) {
	return a.UnzipToMapWithContext(context.Background(), src, filters...)
}

// UnzipToMap extracts all files from the archive into a map[filename]content with context support.
func (a *Archiver) UnzipToMapWithContext(ctx context.Context, src Source, filters ...Filter) (m map[string][]byte, err error) {
	archive := a.newZip()
	if err := a.loadZip(ctx, src, archive); err != nil {
		return nil, err
	}
	defer func() {
		closeErr := src.Close()
		if err == nil {
			err = closeErr
		}
	}()

	files := archive.applyFilters(archive.Files(), filters)
	m = make(map[string][]byte, len(files))

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if f.IsDir() {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name(), err)
		}

		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name(), err)
		}

		m[f.Name()] = data
	}

	return
}

// UnzipToTemp unpacks the archive into a temporary directory.
// It returns the path and a cleanup function.
// It is the caller's responsibility to call cleanup().
func (a *Archiver) UnzipToTemp(zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	return a.UnzipToTempWithContext(context.Background(), zip, prefix, opts...)
}

// UnzipToTempWithContext extracts the archive to a temporary directory with cancellation support.
func (a *Archiver) UnzipToTempWithContext(ctx context.Context, zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	path, err = os.MkdirTemp("", prefix)
	if err != nil {
		return
	}
	cleanup = func() { _ = os.RemoveAll(path) }

	if err = archive.ExtractToWithContext(ctx, path, opts...); err != nil {
		cleanup()
		return "", nil, err
	}

	return
}

// UnzipGlob extracts all files file whose name matches the [path.Match] pattern.
func (a *Archiver) UnzipGlob(zip Source, pattern, destDir string) error {
	return a.UnzipGlobWithContext(context.Background(), zip, pattern, destDir)
}

// UnzipGlobWithContext extracts all files file whose name matches the [path.Match] pattern with context support.
func (a *Archiver) UnzipGlobWithContext(ctx context.Context, zip Source, pattern, destDir string) (err error) {
	archive := a.newZip()
	if err = a.loadZip(ctx, zip, archive); err != nil {
		return
	}
	defer func() {
		closeErr := zip.Close()
		if err == nil {
			err = closeErr
		}
	}()

	files, err := archive.Glob(pattern)
	if err != nil {
		return
	}

	err = archive.ExtractTo(destDir, WithOnly(files))
	return
}

func (a *Archiver) openFile(filename string, archive *Zip) (io.ReadCloser, error) {
	f, ok := archive.File(filename)
	if !ok {
		return nil, ErrFileNotFound
	}
	return f.Open()
}

func (a *Archiver) loadZip(ctx context.Context, zip Source, archive *Zip) error {
	rc, size, err := zip.Open()
	if err != nil {
		return err
	}

	_, err = archive.LoadWithContext(ctx, rc, size)
	return nil
}

func (a *Archiver) applyFilters(files []*File, filters []Filter) []*File {
	for _, filter := range filters {
		files = filter(files)
	}
	return files
}

var DefaultArchiver = NewArchiver()

// ReadFile reads file content using the default archiver. See [Archiver.ReadFile].
func ReadFile(zip Source, filename string) ([]byte, error) {
	return DefaultArchiver.ReadFile(zip, filename)
}

// ReplaceFile replaces a file. See [Archiver.ReplaceFile]
func ReplaceFile(src Source, dest Sink, filename string, content io.Reader) error {
	return DefaultArchiver.ReplaceFile(src, dest, filename, content)
}

// GetEntries lists files using the default archiver. [Archiver.GetEntries]
func GetEntries(zip Source) ([]*File, error) {
	return DefaultArchiver.GetEntries(zip)
}

// Verify checks the integrity of the archive. See [Archiver.Verify].
func Verify(zip Source, opts ...ZipOption) error {
	return DefaultArchiver.Verify(zip, opts...)
}

// Exists checks file existence. See [Archiver.Exists].
func Exists(zip Source, filename string) (bool, error) {
	return DefaultArchiver.Exists(zip, filename)
}

// Walk iterates the archive. See [Archiver.Walk].
func Walk(zip Source, walkFn func(*File) error, filters ...Filter) error {
	return DefaultArchiver.Walk(zip, walkFn, filters...)
}

// Search grep content. See [Archiver.Search].
func Search(zip Source, text string, filters ...Filter) ([]*File, error) {
	return DefaultArchiver.Search(zip, text, filters...)
}

// UpdateMetadata modifies attributes. See [Archiver.UpdateMetadata].
func UpdateMetadata(zip Source, dest Sink, modifier func(*File), opts ...ZipOption) error {
	return DefaultArchiver.UpdateMetadata(zip, dest, modifier, opts...)
}

// Transform modifies files on the fly. See [Archiver.Transform]
func Transform(src Source, dest Sink, fn TransformFunc, opts ...ZipOption) error {
	return DefaultArchiver.Transform(src, dest, fn, opts...)
}

// Diff compares archives. See [Archiver.Diff]
func Diff(srcA, srcB Source, filters ...Filter) (ArchiveDiff, error) {
	return DefaultArchiver.Diff(srcA, srcB, filters...)
}

// Tree returns a string representation of the archive structure.  See [Archiver.Tree].
func Tree(src Source, filters ...Filter) (string, error) {
	return DefaultArchiver.Tree(src, filters...)
}

// TotalSize calculates sizes using the default archiver. See [Archiver.TotalSize].
func TotalSize(src Source, filters ...Filter) (uncompressed, compressed int64, err error) {
	return DefaultArchiver.TotalSize(src, filters...)
}

// IsEncrypted checks encryption using the default archiver. See [Archiver.IsEncrypted].
func IsEncrypted(src Source, filters ...Filter) (bool, error) {
	return DefaultArchiver.IsEncrypted(src, filters...)
}

// ArchiveDir recursively adds contents of the directory to the archive and writes it to dest. See [Archiver.ArchiveDir].
func ArchiveDir(srcDir string, dest Sink, opts ...ZipOption) error {
	return DefaultArchiver.ArchiveDir(srcDir, dest, opts...)
}

// ArchiveFiles adds files to the archive and writes it to dest. See [Archiver.ArchiveFiles].
func ArchiveFiles(files []string, dest Sink, opts ...ZipOption) error {
	return DefaultArchiver.ArchiveFiles(files, dest, opts...)
}

// Merge combines archives using the default archiver. See [Archiver.Merge].
func Merge(dest Sink, sources ...Source) error {
	return DefaultArchiver.Merge(dest, sources...)
}

// Clone copies archive with options using the default archiver. See [Archiver.Clone].
func Clone(zip Source, dest Sink, opts ...ZipOption) error {
	return DefaultArchiver.Clone(zip, dest, opts...)
}

// Unzip opens source zip and extracts its contents to dest directory. See [Archiver.Unzip].
func Unzip(zip Source, destDir string, opts ...ZipOption) error {
	return DefaultArchiver.Unzip(zip, destDir, opts...)
}

// UnzipFile extracts the specified file from the archive. See [Archiver.UnzipFile].
func UnzipFile(zip Source, filename string, dest Sink) error {
	return DefaultArchiver.UnzipFile(zip, filename, dest)
}

// UnzipToMap loads archive content into memory. See [Archiver.UnzipToMap].
func UnzipToMap(src Source, filters ...Filter) (map[string][]byte, error) {
	return DefaultArchiver.UnzipToMap(src, filters...)
}

// UnzipToTemp extracts to a temp dir. See [Archiver.UnzipToTemp].
func UnzipToTemp(zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	return DefaultArchiver.UnzipToTemp(zip, prefix, opts...)
}

// UnzipGlob extracts all files file whose name matches the [path.Match] pattern. See [Archiver.UnzipGlob].
func UnzipGlob(src Source, pattern, destDir string) error {
	return DefaultArchiver.UnzipGlob(src, pattern, destDir)
}

type fileManager struct {
	path string
	f    *os.File
}

func (s *fileManager) Open() (io.ReaderAt, int64, error) {
	if s.f == nil {
		f, err := os.Open(s.path)
		if err != nil {
			return nil, 0, err
		}
		s.f = f
	}
	stat, err := s.f.Stat()
	return s.f, stat.Size(), err
}

func (s *fileManager) Create() (io.Writer, error) {
	if s.f == nil {
		if dir := filepath.Dir(s.path); dir != "." {
			os.MkdirAll(dir, 0755)
		}
		f, err := os.Create(s.path)
		if err != nil {
			return nil, err
		}
		s.f = f
	}
	return s.f, nil
}

func (s *fileManager) Close() error {
	if s.path == "" {
		return nil
	}

	err := s.f.Close()
	s.f = nil // Allow reusing
	return err
}

type readerManager struct {
	r    io.ReaderAt
	size int64
}

func (s readerManager) Open() (io.ReaderAt, int64, error) {
	return s.r, s.size, nil
}

func (s readerManager) Close() error {
	return nil
}

type writerManager struct {
	w io.Writer
}

func (s writerManager) Create() (io.Writer, error) {
	return s.w, nil
}

func (s writerManager) Close() error {
	if closer, ok := s.w.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type httpSource struct {
	url    string
	client *http.Client
}

func (h *httpSource) Open() (io.ReaderAt, int64, error) {
	req, err := http.NewRequest("HEAD", h.url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("failed to fetch metadata: %s", resp.Status)
	}

	if resp.Header.Get("Accept-Ranges") != "bytes" && resp.Header.Get("Content-Length") == "" {
		return nil, 0, fmt.Errorf("server does not support Range requests or Content-Length is missing")
	}

	return &httpReaderAt{url: h.url, client: h.client}, resp.ContentLength, nil
}

func (h *httpSource) Close() error { return nil }

// httpReaderAt implements io.ReaderAt via HTTP Range requests
type httpReaderAt struct {
	url    string
	client *http.Client
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return 0, err
	}

	end := off + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	return io.ReadFull(resp.Body, p)
}
