package gozip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Source provides an interface for reading a ZIP archive.
// Implementations must support multiple reads (for example, via io.ReaderAt).
type Source interface {
	Open() (io.Reader, int64, error)
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

// FromStream creates source from an [io.Reader] with a known size.
func FromStream(r io.Reader, size int64) Source { return readerManager{r, size} }

// FromReaderAt creates a Source from an [io.ReaderAt] with a known size.
func FromReaderAt(r io.ReaderAt, size int64) Source { return readerAtManager{r, size} }

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

// ToURL creates a Sink that streams the generated ZIP archive directly to a URL.
// If method is empty, it defaults to [http.MethodPut].
func ToURL(url, method string, client *http.Client) Sink {
	if method == "" {
		method = http.MethodPut
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &httpSink{
		url:    url,
		method: method,
		client: client,
		errCh:  make(chan error, 1),
	}
}

// UseSource opens the source, handles the ReaderAt vs Reader distinction,
// and ensures resources are closed properly after fn executes.
//
// It passes:
//   - r: The underlying stream (always non-nil).
//   - rAt: The Random Access interface (non-nil if supported, e.g. File or Memory).
//   - size: The total size (or [SizeUnknown]).
func UseSource(src Source, fn func(r io.Reader, rAt io.ReaderAt, size int64) error) (err error) {
	r, size, err := src.Open()
	if err != nil {
		return
	}
	defer func() {
		closeErr := src.Close()
		if err == nil {
			err = closeErr
		}
	}()

	var rAt io.ReaderAt
	if ra, ok := r.(io.ReaderAt); ok {
		rAt = ra
	}

	err = fn(r, rAt, size)
	return
}

// UseSink creates the destination writer and ensures it is closed properly.
func UseSink(sink Sink, fn func(w io.Writer) error) (err error) {
	w, err := sink.Create()
	if err != nil {
		return err
	}
	defer func() {
		closeErr := sink.Close()
		if err == nil {
			err = closeErr
		}
	}()

	err = fn(w)
	return
}

// SafePath returns a clean, absolute path for a zip entry within the destination directory.
// It ensures that the resulting path is inside the destDir (prevents Zip Slip).
func SafePath(destDir, fileName string) (string, error) {
	cleanedName := strings.TrimPrefix(path.Clean("/"+fileName), "/")

	for _, r := range fileName {
		if r < 0x20 || r == 0x7F {
			return "", fmt.Errorf("%w: filename contains control characters", ErrInsecurePath)
		}
	}

	destDir = filepath.Clean(destDir)
	fullPath := filepath.Join(destDir, filepath.FromSlash(cleanedName))

	rel, err := filepath.Rel(destDir, fullPath)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("%w: path escapes destination directory", ErrInsecurePath)
	}

	return fullPath, nil
}

// Archiver provides a configurable environment for working with ZIP archives.
// It is build on top of [Zip] and reduces boilerplate code for common operations.
// By default, it supports the [Store] (no compression) and [Deflate] compression methods.
type Archiver struct {
	engineOptions []ArchiveOption
}

// NewArchiver creates a new Archiver instance build on top of [Zip] with the specified options.
func NewArchiver(opts ...ArchiveOption) *Archiver {
	return &Archiver{engineOptions: opts}
}

// newZip creates a new Zip instance with the specified options applied.
func (a *Archiver) newZip(opts ...ArchiveOption) *Zip {
	return NewZip(append(a.engineOptions, opts...)...)
}

// ReadFile reads the contents of a file from the archive into a byte slice.
func (a *Archiver) ReadFile(zip Source, filename string) ([]byte, error) {
	return a.ReadFileWithContext(context.Background(), zip, filename)
}

// ReadFileWithContext reads file content with context support.
// Cancelling the context stops the reading process.
func (a *Archiver) ReadFileWithContext(ctx context.Context, zip Source, filename string) ([]byte, error) {
	var data []byte

	err := UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			if _, err := archive.LoadWithContext(ctx, rAt, size); err != nil {
				return err
			}

			rc, err := a.openFile(filename, archive)
			if err != nil {
				return err
			}

			data, err = io.ReadAll(rc)
			return err
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		_, err := sr.Scan(filename)
		if err != nil {
			return err
		}

		rc, err := sr.Open()
		if err != nil {
			return err
		}

		data, err = io.ReadAll(rc)
		return err
	})

	return data, err
}

// ReplaceFile replaces a file in the archive with new content.
// The other files are copied without changes.
func (a *Archiver) ReplaceFile(zip Source, dest Sink, filename string, content io.Reader) error {
	return a.ReplaceFileWithContext(context.Background(), zip, dest, filename, content)
}

// ReplaceFileWithContext replaces a file in the archive with new content with context support.
func (a *Archiver) ReplaceFileWithContext(ctx context.Context, zip Source, dest Sink, filename string, content io.Reader) error {
	return a.TransformWithContext(ctx, zip, dest, func(f *File) (io.Reader, error) {
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
// If source is [io.Reader], the returned files cannot be opened.
func (a *Archiver) GetEntries(zip Source) ([]*File, error) {
	return a.GetEntriesWithContext(context.Background(), zip)
}

// GetEntriesWithContext lists files with context support.
func (a *Archiver) GetEntriesWithContext(ctx context.Context, zip Source) ([]*File, error) {
	var files []*File

	err := UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			var err error
			files, err = a.newZip().LoadWithContext(ctx, rAt, size)
			return err
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))

		for {
			f, err := sr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			files = append(files, f)
		}
	})

	return files, err
}

// Verify checks the integrity of the files in the archive (CRC, checksums).
// If source is [io.Reader], zip options do not apply.
func (a *Archiver) Verify(zip Source, opts ...ZipOption) error {
	return a.VerifyWithContext(context.Background(), zip, opts...)
}

// Verify checks the integrity of the files in the archive with context support.
func (a *Archiver) VerifyWithContext(ctx context.Context, zip Source, opts ...ZipOption) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			archive.LoadWithContext(ctx, rAt, size)
			return archive.VerifyWithContext(ctx, opts...)
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		var errs []error

		for {
			_, err := sr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				errs = append(errs, err)
				break
			}

			r, err = sr.Open()
			if err != nil {
				errs = append(errs, err)
				continue
			}

			if _, err = io.Copy(io.Discard, r); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	})
}

// Exists checks for the presence of a file in the archive.
func (a *Archiver) Exists(zip Source, filename string) (bool, error) {
	return a.ExistsWithContext(context.Background(), zip, filename)
}

// ExistsWithContext checks if a file exists with context support.
func (a *Archiver) ExistsWithContext(ctx context.Context, zip Source, filename string) (bool, error) {
	var exists bool

	err := UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			if _, err := archive.LoadWithContext(ctx, rAt, size); err != nil {
				return err
			}
			exists = archive.Exists(filename)
			return nil
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))

		for {
			f, err := sr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if f.Name() == filename {
				exists = true
				return nil
			}
		}
	})

	return exists, err
}

// Walk traverses all files in the archive, calling walkFn for each one.
// If walkFn returns an error, the traversal stops.
func (a *Archiver) Walk(zip Source, walkFn func(*File) error, filters ...Filter) error {
	return a.WalkWithContext(context.Background(), zip, walkFn, filters...)
}

// WalkWithContext iterates over the archive with context cancellation support.
func (a *Archiver) WalkWithContext(ctx context.Context, zip Source, walkFn func(*File) error, filters ...Filter) (err error) {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			_, err := archive.LoadWithContext(ctx, rAt, size)
			if err != nil {
				return err
			}

			for _, f := range archive.Select(filters...) {
				if err = ctx.Err(); err != nil {
					return err
				}
				if err = walkFn(f); err != nil {
					return err
				}
			}
			return nil
		}

		sr := NewStreamReader(r)

		for {
			f, err := sr.Next()
			if err != nil {
				return err
			}

			isMatch := true
			for _, filter := range filters {
				if !filter(f) {
					isMatch = false
					break
				}
			}

			if !isMatch {
				continue
			}

			// Allow file to be opened in walkFn
			size := f.UncompressedSize()
			f.WithOpenFunc(sr.Open).WithUncompressedSize(size)

			if err = walkFn(f); err != nil {
				return err
			}
		}
	})
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
// Returns [ErrNotImplemented] if source is [io.Reader].
func (a *Archiver) UpdateMetadata(zip Source, dest Sink, modifier func(*File), opts ...ZipOption) error {
	return a.UpdateMetadataWithContext(context.Background(), zip, dest, modifier, opts...)
}

// UpdateMetadataWithContext allows bulk modification with context support.
func (a *Archiver) UpdateMetadataWithContext(ctx context.Context, zip Source, dest Sink, modifier func(*File), opts ...ZipOption) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt == nil {
			return fmt.Errorf("%w: UpdateMetadata only accepts io.ReaderAt", ErrNotImplemented)
		}

		archive := a.newZip()
		files, err := archive.LoadWithContext(ctx, rAt, size)
		if err != nil {
			return err
		}

		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			modifier(f)
		}

		return UseSink(dest, func(w io.Writer) error {
			_, err = archive.WriteToWithContext(ctx, w, opts...)
			return err
		})
	})
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
//
// Returns [ErrNotImplemented] if source is [io.Reader].
func (a *Archiver) Transform(zip Source, dest Sink, fn TransformFunc, opts ...ZipOption) error {
	return a.TransformWithContext(context.Background(), zip, dest, fn, opts...)
}

// TransformWithContext applies a function to each file in the archive, allowing modification of content with context support.
func (a *Archiver) TransformWithContext(ctx context.Context, zip Source, dest Sink, fn TransformFunc, opts ...ZipOption) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt == nil {
			return fmt.Errorf("%w: Transform only accepts io.ReaderAt", ErrNotImplemented)
		}

		files, err := a.newZip().LoadWithContext(ctx, rAt, size)
		if err != nil {
			return err
		}

		output := a.newZip()
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}

			newContent, err := fn(file)
			if err != nil {
				return err
			}

			if newContent != nil {
				output.AddReader(newContent, file.Name(), SizeUnknown)
			} else {
				output.Add(file)
			}
		}

		return UseSink(dest, func(w io.Writer) error {
			_, err = output.WriteToWithContext(ctx, w, opts...)
			return err
		})
	})
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

// DiffWithContext compares two archives and returns the differences with context support.
func (a *Archiver) DiffWithContext(ctx context.Context, zipA, zipB Source, filters ...Filter) (diff ArchiveDiff, err error) {
	filesA, err := a.GetEntriesWithContext(ctx, zipA)
	filesB, err := a.GetEntriesWithContext(ctx, zipB)

	mapA := make(map[string]*File)
	for _, f := range a.applyFilters(filesA, filters) {
		mapA[f.Name()] = f
	}

	mapB := make(map[string]*File)
	for _, f := range a.applyFilters(filesB, filters) {
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
func (a *Archiver) Tree(zip Source, filters ...Filter) (string, error) {
	return a.TreeWithContext(context.Background(), zip, filters...)
}

// Tree returns a string representation of the archive structure with context support.
func (a *Archiver) TreeWithContext(ctx context.Context, zip Source, filters ...Filter) (string, error) {
	files, err := a.GetEntriesWithContext(ctx, zip)
	if err != nil {
		return "", err
	}
	return generateTree(a.applyFilters(files, filters)), nil
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
func (a *Archiver) ArchiveDirWithContext(ctx context.Context, srcDir string, destZip Sink, opts ...ZipOption) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrFileEntry, srcDir)
	}

	archive := a.newZip()
	if _, err = archive.AddDir(srcDir); err != nil {
		return err
	}

	return UseSink(destZip, func(w io.Writer) error {
		_, err := archive.WriteToWithContext(ctx, w, opts...)
		return err
	})
}

// ArchiveFiles archives a list of files.
// File paths are "flattened" and placed in the root of the archive.
func (a *Archiver) ArchiveFiles(files []string, destZip Sink, opts ...ZipOption) error {
	return a.ArchiveFilesWithContext(context.Background(), files, destZip, opts...)
}

// ArchiveFilesWithContext creates an archive from a list of files with context support.
func (a *Archiver) ArchiveFilesWithContext(ctx context.Context, files []string, destZip Sink, opts ...ZipOption) error {
	archive := a.newZip()

	for _, file := range files {
		if _, err := archive.AddFile(file, WithName(filepath.Base(file))); err != nil {
			return err
		}
	}

	return UseSink(destZip, func(w io.Writer) error {
		_, err := archive.WriteToWithContext(ctx, w, opts...)
		return err
	})
}

// Merge combines multiple archives into one.
// Conflict resolution strategy: "the last written file wins".
// Returns [ErrNotImplemented] if source is [io.Reader].
func (a *Archiver) Merge(destZip Sink, sources ...Source) error {
	return a.MergeWithContext(context.Background(), destZip, sources...)
}

// MergeWithContext combines archives with context support.
func (a *Archiver) MergeWithContext(ctx context.Context, destZip Sink, sources ...Source) error {
	archive := a.newZip()

	for _, src := range sources {
		err := UseSource(src, func(r io.Reader, rAt io.ReaderAt, size int64) error {
			if rAt == nil {
				return fmt.Errorf("%w: Merge only accepts io.ReaderAt", ErrNotImplemented)
			}
			_, err := archive.LoadWithContext(ctx, rAt, size)
			return err
		})
		if err != nil {
			return err
		}
	}

	return UseSink(destZip, func(w io.Writer) error {
		_, err := archive.WriteToWithContext(ctx, w)
		return err
	})
}

// Clone copies the archive with the possibility of applying filters or modifiers.
// Example: deleting files, changing compression.
// Returns [ErrNotImplemented] if source is [io.Reader].
func (a *Archiver) Clone(zip Source, destZip Sink, opts ...ZipOption) error {
	return a.CloneWithContext(context.Background(), zip, destZip, opts...)
}

// CloneWithContext copies archive with context support.
func (a *Archiver) CloneWithContext(ctx context.Context, zip Source, destZip Sink, opts ...ZipOption) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt == nil {
			return fmt.Errorf("%w: Clone only accepts io.ReaderAt", ErrNotImplemented)
		}

		archive := a.newZip()
		if _, err := archive.LoadWithContext(ctx, rAt, size); err != nil {
			return err
		}

		return UseSink(destZip, func(w io.Writer) error {
			_, err := archive.WriteToWithContext(ctx, w, opts...)
			return err
		})
	})
}

// Unzip extracts the archive to the specified directory.
// If source is [io.Reader], zip options do not apply.
func (a *Archiver) Unzip(zip Source, destDir string, opts ...ZipOption) error {
	return a.UnzipWithContext(context.Background(), zip, destDir, opts...)
}

// UnzipWithContext extracts archive contents with context support.
// Cancelling the context stops the extraction immediately.
func (a *Archiver) UnzipWithContext(ctx context.Context, zip Source, destDir string, opts ...ZipOption) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()

			if _, err := archive.LoadWithContext(ctx, rAt, size); err != nil {
				return err
			}

			return archive.ExtractToWithContext(ctx, destDir, opts...)
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		return sr.ExtractToWithContext(ctx, destDir)
	})
}

// UnzipFile extracts the specified file from the archive.
func (a *Archiver) UnzipFile(zip Source, filename string, dest Sink) error {
	return a.UnzipFileWithContext(context.Background(), zip, filename, dest)
}

// UnzipFileWithContext extracts the specified file from the archive with context support.
func (a *Archiver) UnzipFileWithContext(ctx context.Context, zip Source, filename string, dest Sink) error {
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			if _, err := archive.LoadWithContext(ctx, rAt, size); err != nil {
				return err
			}

			file, err := a.openFile(filename, archive)
			if err != nil {
				return err
			}

			return UseSink(dest, func(w io.Writer) error {
				_, err = io.Copy(w, file)
				return err
			})
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		_, err := sr.Scan(filename)
		if err != nil {
			return err
		}

		rc, err := sr.Open()
		if err != nil {
			return err
		}

		return UseSink(dest, func(w io.Writer) error {
			_, err = io.Copy(w, rc)
			return err
		})
	})
}

// UnzipToMap extracts all files from the archive into a map[filename]content. Directories are ignored.
func (a *Archiver) UnzipToMap(zip Source, filters ...Filter) (map[string][]byte, error) {
	return a.UnzipToMapWithContext(context.Background(), zip, filters...)
}

// UnzipToMap extracts all files from the archive into a map[filename]content with context support.
func (a *Archiver) UnzipToMapWithContext(ctx context.Context, zip Source, filters ...Filter) (map[string][]byte, error) {
	var m map[string][]byte
	var errs []error

	err := UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			files, err := a.newZip().LoadWithContext(ctx, rAt, size)
			if err != nil {
				return err
			}

			m = make(map[string][]byte, len(files))
			for _, f := range files {
				if err := ctx.Err(); err != nil {
					return err
				}

				if f.IsDir() {
					continue
				}

				rc, err := f.Open()
				if err != nil {
					errs = append(errs, fmt.Errorf("open %s: %w", f.Name(), err))
					continue
				}

				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					errs = append(errs, fmt.Errorf("read %s: %w", f.Name(), err))
					continue
				}

				m[f.Name()] = data
				return nil
			}
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		m = make(map[string][]byte)

		for {
			f, err := sr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			rc, err := sr.Open()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			data, err := io.ReadAll(rc)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			m[f.Name()] = data
		}
	})

	if err != nil {
		errs = append(errs, err)
	}

	return m, errors.Join(errs...)
}

// UnzipToTemp unpacks the archive into a temporary directory.
// It returns the path and a cleanup function.
// It is the caller's responsibility to call cleanup().
func (a *Archiver) UnzipToTemp(zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	return a.UnzipToTempWithContext(context.Background(), zip, prefix, opts...)
}

// UnzipToTempWithContext extracts the archive to a temporary directory with cancellation support.
func (a *Archiver) UnzipToTempWithContext(ctx context.Context, zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	path, err = os.MkdirTemp("", prefix)
	if err != nil {
		return
	}
	cleanup = func() { _ = os.RemoveAll(path) }

	if err = a.Unzip(zip, path, opts...); err != nil {
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
	return UseSource(zip, func(r io.Reader, rAt io.ReaderAt, size int64) error {
		if rAt != nil {
			archive := a.newZip()
			archive.LoadWithContext(ctx, rAt, size)

			files, err := archive.Glob(pattern)
			if err != nil {
				return err
			}

			return archive.ExtractTo(destDir, WithOnly(files))
		}

		sr := NewStreamReader(r, WithStreamConfig(a.newZip().Config()))
		var errs []error

		for {
			f, err := sr.Scan(pattern)
			if err == io.EOF {
				break
			}
			if err != nil {
				errs = append(errs, err)
				break
			}

			fpath, err := SafePath(destDir, f.name)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			if err := sr.ExtractFile(fpath); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	})
}

func (a *Archiver) openFile(filename string, archive *Zip) (io.ReadCloser, error) {
	f, ok := archive.File(filename)
	if !ok {
		return nil, ErrFileNotFound
	}
	return f.Open()
}

func (a *Archiver) applyFilters(files []*File, filters []Filter) []*File {
	n := 0
	for _, f := range files {
		isMatch := true
		for _, filter := range filters {
			if !filter(f) {
				isMatch = false
				break
			}
		}

		if isMatch {
			files[n] = f
			n++
		}
	}

	for i := n; i < len(files); i++ {
		files[i] = nil
	}

	return files[:n]
}

var DefaultArchiver = NewArchiver(
	WithZipConfig(ZipConfig{
		CompressionMethod: Deflate,
		CompressionLevel:  DeflateNormal,
		FileSortStrategy:  SortZIP64Optimized,
	}),
)

// ReadFile reads file content using the default archiver. See [Archiver.ReadFile].
func ReadFile(zip Source, filename string) ([]byte, error) {
	return DefaultArchiver.ReadFile(zip, filename)
}

// ReplaceFile replaces a file. See [Archiver.ReplaceFile]
func ReplaceFile(zip Source, dest Sink, filename string, content io.Reader) error {
	return DefaultArchiver.ReplaceFile(zip, dest, filename, content)
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
func Transform(zip Source, dest Sink, fn TransformFunc, opts ...ZipOption) error {
	return DefaultArchiver.Transform(zip, dest, fn, opts...)
}

// Diff compares archives. See [Archiver.Diff]
func Diff(srcA, srcB Source, filters ...Filter) (ArchiveDiff, error) {
	return DefaultArchiver.Diff(srcA, srcB, filters...)
}

// Tree returns a string representation of the archive structure.  See [Archiver.Tree].
func Tree(zip Source, filters ...Filter) (string, error) {
	return DefaultArchiver.Tree(zip, filters...)
}

// TotalSize calculates sizes using the default archiver. See [Archiver.TotalSize].
func TotalSize(zip Source, filters ...Filter) (uncompressed, compressed int64, err error) {
	return DefaultArchiver.TotalSize(zip, filters...)
}

// IsEncrypted checks encryption using the default archiver. See [Archiver.IsEncrypted].
func IsEncrypted(zip Source, filters ...Filter) (bool, error) {
	return DefaultArchiver.IsEncrypted(zip, filters...)
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
func UnzipToMap(zip Source, filters ...Filter) (map[string][]byte, error) {
	return DefaultArchiver.UnzipToMap(zip, filters...)
}

// UnzipToTemp extracts to a temp dir. See [Archiver.UnzipToTemp].
func UnzipToTemp(zip Source, prefix string, opts ...ZipOption) (path string, cleanup func(), err error) {
	return DefaultArchiver.UnzipToTemp(zip, prefix, opts...)
}

// UnzipGlob extracts all files file whose name matches the [path.Match] pattern. See [Archiver.UnzipGlob].
func UnzipGlob(zip Source, pattern, destDir string) error {
	return DefaultArchiver.UnzipGlob(zip, pattern, destDir)
}

type fileManager struct {
	path string
	f    *os.File
}

func (s *fileManager) Open() (io.Reader, int64, error) {
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

type readerAtManager struct {
	r    io.ReaderAt
	size int64
}

func (s readerAtManager) Open() (io.Reader, int64, error) {
	return &readerAtWrapper{r: s.r}, s.size, nil
}

func (s readerAtManager) Close() error { return nil }

type readerManager struct {
	r    io.Reader
	size int64
}

func (s readerManager) Open() (io.Reader, int64, error) {
	return s.r, s.size, nil
}

func (s readerManager) Close() error { return nil }

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

type readerAtWrapper struct {
	r      io.ReaderAt
	offset int64
}

func (s *readerAtWrapper) Read(p []byte) (n int, err error) {
	n, err = s.r.ReadAt(p, s.offset)
	s.offset += int64(n)
	return
}

func (s *readerAtWrapper) ReadAt(p []byte, off int64) (n int, err error) {
	return s.r.ReadAt(p, off)
}

type httpSource struct {
	url    string
	client *http.Client
}

func (h *httpSource) Open() (io.Reader, int64, error) {
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

	return &httpReader{url: h.url, client: h.client}, resp.ContentLength, nil
}

func (h *httpSource) Close() error { return nil }

// httpReader implements io.ReaderAt and io.Reader via HTTP Range requests
type httpReader struct {
	url    string
	client *http.Client
	offset int64
}

func (r *httpReader) ReadAt(p []byte, off int64) (n int, err error) {
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

func (w *httpReader) Read(p []byte) (n int, err error) {
	n, err = w.ReadAt(p, w.offset)
	w.offset += int64(n)
	return
}

// httpSink implements the Sink interface for uploading data to a URL.
type httpSink struct {
	url    string
	method string
	client *http.Client

	pw    *io.PipeWriter
	errCh chan error
}

func (s *httpSink) Create() (io.Writer, error) {
	pr, pw := io.Pipe()
	s.pw = pw

	go func() {
		req, err := http.NewRequest(s.method, s.url, pr)
		if err != nil {
			s.errCh <- err
			pr.CloseWithError(err)
			return
		}

		req.Header.Set("Content-Type", "application/zip")

		resp, err := s.client.Do(req)
		if err != nil {
			s.errCh <- err
			pr.CloseWithError(err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			err = fmt.Errorf("upload failed with status: %s", resp.Status)
			s.errCh <- err
			pr.CloseWithError(err)
			return
		}

		s.errCh <- nil
	}()

	return pw, nil
}

func (s *httpSink) Close() error {
	if s.pw != nil {
		err := s.pw.Close()

		httpErr, _ := <-s.errCh
		if err == nil {
			err = httpErr
		}
		return err
	}
	return nil
}
