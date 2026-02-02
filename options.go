package gozip

import (
	"io"
	"io/fs"
	"path"
	"strings"
)

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

type processConfig struct {
	filters    []Filter
	workers    int
	onProgress func(stats ProgressStats)
}

type Option func(*processConfig)

// WithWorkers sets the given amount of workers for the operation.
func WithWorkers(n int) Option {
	return func(pc *processConfig) {
		if n <= 0 {
			pc.workers = 1
		} else {
			pc.workers = n
		}
	}
}

// WithFilter allows using any custom [Filter] as an option.
func WithFilter(f Filter) Option {
	return func(c *processConfig) {
		if f != nil {
			c.filters = append(c.filters, f)
		}
	}
}

// Filter filters out the files.
type Filter func(files []*File) []*File

// WithFiles filters the operation to only the specific files provided.
func WithFiles(files []*File) Filter {
	return func(_ []*File) []*File { return files }
}

// FromDir restricts operation to files nested under the specified path.
func FromDir(dirPath string) Option {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, func(files []*File) []*File {
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
		})
	}
}

// WithoutDir excludes a directory and its contents from operation.
func WithoutDir(dirPath string) Option {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, func(files []*File) []*File {
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
		})
	}
}

// DefaultStoreExtensions is an extended list of formats that are
// already compressed and do not require reprocessing.
var DefaultStoreExtensions = map[string]struct{}{
	"7z": {}, "aar": {}, "ace": {}, "apk": {}, "arc": {}, "arj": {},
	"br": {}, "bz2": {}, "cab": {}, "deb": {}, "dmg": {}, "epub": {},
	"gz": {}, "jar": {}, "lz4": {}, "lzma": {}, "lzo": {}, "rar": {},
	"rpm": {}, "tar": {}, "tgz": {}, "war": {}, "xz": {}, "zip": {},
	"zst": {},
	// Media Files
	"mp4": {}, "mkv": {}, "avi": {}, "mov": {}, "webm": {},
	"jpg": {}, "jpeg": {}, "png": {}, "gif": {}, "webp": {},
	"mp3": {}, "ogg": {}, "flac": {},
	// Documents
	"pdf": {}, "docx": {}, "xlsx": {}, "pptx": {},
}

// WithSmartStore disables compression for files whose extensions are in the list.
// Passed extensions are merged with [DefaultStoreExtensions].
// This filter changes the state of [File] objects in the archive.
func WithSmartStore(exts ...string) Option {
	extMap := make(map[string]struct{}, len(DefaultStoreExtensions)+len(exts))
	for k := range DefaultStoreExtensions {
		extMap[k] = struct{}{}
	}
	for _, ext := range exts {
		extMap[strings.ToLower(strings.TrimPrefix(ext, "."))] = struct{}{}
	}
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, func(files []*File) []*File {
			for _, f := range files {
				if f.isDir {
					continue
				}
				ext := strings.ToLower(strings.TrimPrefix(path.Ext(f.name), "."))
				if _, ok := extMap[ext]; ok {
					f.SetCompression(Store, 0)
				}
			}
			return files
		})
	}
}

// Predicate defines the condition for selecting a file.
type Predicate func(*File) bool

// Where returns a filtering option based on an arbitrary condition.
// This is the most flexible way to select files.
func Where(cond Predicate) Option {
	return WithFilter(func(files []*File) []*File {
		n := 0
		for _, f := range files {
			if cond(f) {
				files[n] = f
				n++
			}
		}
		for i := range len(files) {
			files[i] = nil
		}
		return files[:n]
	})
}

// ProgressStats contains detailed information about the current progress of the operation.
type ProgressStats struct {
	CurrentFile       *File
	CurrentRead       int64 // Uncompressed bytes read from current file
	CurrentCompressed int64 // Compressed/Encrypted bytes produced for current file
	ExpectedRead      int64 // Sum of uncompressed file sizes
	TotalRead         int64 // Total uncompressed bytes read so far
	TotalCompressed   int64 // Total compressed bytes produced so far
	TotalWritten      int64 // Total bytes written to dest (headers + data + CD)
	TotalFiles        int64
	ProcessedFiles    int64
	Errors            int64
}

// WithProgress adds a callback to monitor bytes in real time.
func WithProgress(cb func(ProgressStats)) Option {
	return func(c *processConfig) {
		c.onProgress = cb
	}
}

// progressReader wraps file reading to update statistics
type progressReader struct {
	r      io.Reader
	f      *File
	onRead func(*File, int)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.onRead(pr.f, n)
	}
	return n, err
}

// progressWriter wraps the write to the archive to update global statistics
type progressWriter struct {
	w       io.Writer
	f       *File
	onWrite func(*File, int)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if n > 0 {
		pw.onWrite(pw.f, n)
	}
	return n, err
}

// progressWriteSeeker is needed if dest supports Seek (e.g., os.File),
// so that zipWriter can return and rewrite the headers.
type progressWriteSeeker struct {
	*progressWriter
	seeker io.Seeker
}

func (pws *progressWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	return pws.seeker.Seek(offset, whence)
}
