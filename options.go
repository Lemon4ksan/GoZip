package gozip

import (
	"io"
	"path"
	"strings"
	"sync/atomic"
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

type processConfig struct {
	filters    []Filter
	workers    int
	onProgress func(stats ProgressStats)
	onFileDone func(*File, error)
}

type ZipOption func(*processConfig)

// WithWorkers sets the given amount of workers for the operation.
func WithWorkers(n int) ZipOption {
	return func(pc *processConfig) {
		if n <= 0 {
			pc.workers = 1
		} else {
			pc.workers = n
		}
	}
}

// WithOnFileDone overrides global [ZipConfig.OnFileDone]
func WithOnFileDone(fn func(*File, error)) ZipOption {
	return func(pc *processConfig) {
		pc.onFileDone = fn
	}
}

// WithFilter allows using any custom [Filter] as an option.
func WithFilter(f Filter) ZipOption {
	return func(c *processConfig) {
		if f != nil {
			c.filters = append(c.filters, f)
		}
	}
}

// Filter filters out the files.
type Filter func(files []*File) []*File

// WithFiles filters the operation to only the specific files provided.
func WithFiles(files []*File) ZipOption {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, func(_ []*File) []*File { return files })
	}
}

// FromDir restricts operation to files nested under the specified path.
func FromDir(dirPath string) ZipOption {
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
func WithoutDir(dirPath string) ZipOption {
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
func WithSmartStore(exts ...string) ZipOption {
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
func Where(cond Predicate) ZipOption {
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
// If operation uses workers, this progress is called concurrently.
func WithProgress(cb func(ProgressStats)) ZipOption {
	return func(c *processConfig) {
		c.onProgress = cb
	}
}

type signalFunc func(f *File, n int, fileRead int64)

// statsCollector encapsulates metrics tracking and thread-safe updates.
type statsCollector struct {
	stats       *ProgressStats
	onProgress  func(ProgressStats)
	onFileDone  func(*File, error) // User defined callback from config
	destCounter *atomicCounterWriter
}

func newStatsCollector(cfg processConfig, files []*File, destCounter *atomicCounterWriter) *statsCollector {
	if cfg.onProgress == nil && cfg.onFileDone == nil {
		return &statsCollector{destCounter: destCounter}
	}

	s := &ProgressStats{TotalFiles: int64(len(files))}
	for _, f := range files {
		s.ExpectedRead += f.UncompressedSize()
	}

	return &statsCollector{
		stats:       s,
		onProgress:  cfg.onProgress,
		onFileDone:  cfg.onFileDone,
		destCounter: destCounter,
	}
}

func (c *statsCollector) OnRead(f *File, n int, fileRead int64) {
	if c.stats == nil {
		return
	}
	atomic.StoreInt64(&c.stats.CurrentRead, fileRead)
	atomic.AddInt64(&c.stats.TotalRead, int64(n))
	if c.destCounter != nil {
		atomic.StoreInt64(&c.stats.TotalWritten, c.destCounter.Count())
	}
	c.notify(f)
}

// OnCompressed handles updates when bytes are compressed.
func (c *statsCollector) OnCompressed(_ *File, n int, compressed int64) {
	if c.stats == nil {
		return
	}
	atomic.AddInt64(&c.stats.TotalCompressed, int64(n))
	atomic.StoreInt64(&c.stats.CurrentCompressed, compressed)
}

// OnFileDone handles completion of a single file processing.
func (c *statsCollector) OnFileDone(f *File, err error) {
	if c.stats != nil {
		atomic.AddInt64(&c.stats.ProcessedFiles, 1)
		if err != nil {
			atomic.AddInt64(&c.stats.Errors, 1)
		}
		c.notify(f)
		atomic.StoreInt64(&c.stats.CurrentRead, 0)
	}

	if c.onFileDone != nil {
		c.onFileDone(f, err)
	}
}

// Finish sends the final progress event.
func (c *statsCollector) Finish() {
	if c.stats != nil && c.onProgress != nil {
		c.notify(nil)
	}
}

// notify creates a snapshot and calls the user callback.
func (c *statsCollector) notify(f *File) {
	if c.onProgress == nil {
		return
	}
	c.onProgress(c.snapshotStats(f))
}

func (c *statsCollector) snapshotStats(current *File) ProgressStats {
	return ProgressStats{
		CurrentFile:       current,
		CurrentRead:       atomic.LoadInt64(&c.stats.CurrentRead),
		CurrentCompressed: atomic.LoadInt64(&c.stats.CurrentCompressed),
		TotalRead:         atomic.LoadInt64(&c.stats.TotalRead),
		TotalCompressed:   atomic.LoadInt64(&c.stats.TotalCompressed),
		ProcessedFiles:    atomic.LoadInt64(&c.stats.ProcessedFiles),
		Errors:            atomic.LoadInt64(&c.stats.Errors),
		ExpectedRead:      c.stats.ExpectedRead, // const
		TotalFiles:        c.stats.TotalFiles,   // const
		TotalWritten:      atomic.LoadInt64(&c.stats.TotalWritten),
	}
}

// progressReader wraps file reading to update statistics
type progressReader struct {
	r         io.Reader
	onRead    func(n int, localTotal int64)
	processed int64
}

func newProgressReader(r io.Reader, f *File, onRead signalFunc) *progressReader {
	return &progressReader{r: r, onRead: func(n int, localTotal int64) {
		onRead(f, n, localTotal)
	}}
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.processed += int64(n)
		if pr.onRead != nil {
			pr.onRead(n, pr.processed)
		}
	}
	return n, err
}

// progressWriter wraps the write to the archive to update global statistics
type progressWriter struct {
	w         io.Writer
	onWrite   func(n int, localTotal int64)
	processed int64
}

func newProgressWriter(w io.Writer, f *File, onWrite signalFunc) *progressWriter {
	return &progressWriter{w: w, onWrite: func(n int, localTotal int64) {
		onWrite(f, n, localTotal)
	}}
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if n > 0 {
		pw.processed += int64(n)
		if pw.onWrite != nil {
			pw.onWrite(n, pw.processed)
		}
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
