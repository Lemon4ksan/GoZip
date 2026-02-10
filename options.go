package gozip

import (
	"fmt"
	"io"
	"path"
	"strings"
	"sync/atomic"
)

// ArchiveOption is a function option for configuring archive creation
type ArchiveOption func(*Zip)

// WithZipConfig applies the complete [ZipConfig] to the archive.
func WithZipConfig(cfg ZipConfig) ArchiveOption {
	return func(z *Zip) {
		z.config = cfg
	}
}

// WithCompressor registers a custom compression algorithm for this archive instance.
func WithCompressor(method CompressionMethod, factory CompressorFactory) ArchiveOption {
	return func(z *Zip) {
		z.RegisterCompressor(method, factory)
	}
}

// WithDecompressor registers a custom decompression algorithm.
func WithDecompressor(method CompressionMethod, d Decompressor) ArchiveOption {
	return func(z *Zip) {
		z.RegisterDecompressor(method, d)
	}
}

// WithCompression sets the compression method and level for a regular file.
func WithZipCompression(c CompressionMethod, lvl int) ArchiveOption {
	return func(z *Zip) {
		z.config.CompressionMethod = c
		z.config.CompressionLevel = lvl
	}
}

// WithPassword sets the encryption password for the archive.
func WithZipEncryption(e EncryptionMethod, pwd string) ArchiveOption {
	return func(z *Zip) {
		z.config.EncryptionMethod = e
		z.config.Password = pwd
	}
}

// WithArchivePasswords sets global password for the archive.
// If no encryption method is specified, it defaults to [AES256].
func WithZipPassword(pwd string) ArchiveOption {
	return func(z *Zip) {
		z.config.Password = pwd
	}
}

// WithImplicitDirs enables writing implicit dirs to the resulting archive.
func WithImplicitDirs() ArchiveOption {
	return func(z *Zip) {
		z.config.IncludeImplicitDirs = true
	}
}

// AddOption is a functional option for configuring file entries during addition.
type AddOption func(f *File)

// WithConfig applies a complete [FileConfig], overwriting existing settings.
func WithConfig(c FileConfig) AddOption {
	return func(f *File) {
		f.WithConfig(c)
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

// WithMarkDirsImplicit marks added directories as implicit.
func WithMarkDirsImplicit() AddOption {
	return func(f *File) {
		if f.isDir {
			f.isImplicit = true
		}
	}
}

type processConfig struct {
	filters    []Filter
	password   string
	workers    int
	onProgress func(stats ProgressStats)
	onFileDone func(*File, error)
	security   SecuritySettings
}

// ZipOption is a function option for configuring [Zip] operations.
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

// WithOpPassword overwrites password for each file in the operation.
func WithOpPassword(pwd string) ZipOption {
	return func(pc *processConfig) {
		pc.password = pwd
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

// WithOnly filters the operation to only the specific files provided.
func WithOnly(files []*File) ZipOption {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, FilterOnly(files))
	}
}

// WithFromDir restricts operation to files nested under the specified path.
func WithFromDir(dirPath string) ZipOption {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, FilterFromDir(dirPath))
	}
}

// WithExcludeDir excludes a directory and its contents from operation.
func WithExcludeDir(dirPath string) ZipOption {
	return func(pc *processConfig) {
		pc.filters = append(pc.filters, FilterExcludeDir(dirPath))
	}
}

// Filter filters out the files.
type Filter func(file *File) bool

// WithFiles returns files provided.
func FilterOnly(files []*File) Filter {
	lookup := make(map[string]struct{})
	for _, file := range files {
		lookup[file.entryName()] = struct{}{}
	}
	return func(file *File) bool {
		_, ok := lookup[file.entryName()]
		return ok
	}
}

// FilterFromDir returns only files nested under the specified path.
func FilterFromDir(dirPath string) Filter {
	prefix := strings.TrimPrefix(path.Clean(strings.ReplaceAll(dirPath, "\\", "/")), "/")
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	dirEntryName := strings.TrimSuffix(prefix, "/")

	return func(file *File) bool {
		if dirPath == "" || dirPath == "." {
			return true
		}
		fName := file.entryName()
		return strings.HasPrefix(fName, prefix) || fName == dirEntryName
	}
}

// FilterExcludeDir returns files without a provided directory and its contents.
func FilterExcludeDir(dirPath string) Filter {
	prefix := strings.TrimPrefix(path.Clean(strings.ReplaceAll(dirPath, "\\", "/")), "/")
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	dirEntryName := strings.TrimSuffix(prefix, "/")

	return func(file *File) bool {
		if dirPath == "" || dirPath == "." {
			return false
		}
		fName := file.entryName()
		return !(strings.HasPrefix(fName, prefix) || fName == dirEntryName)
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
		pc.filters = append(pc.filters, func(file *File) bool {
			if !file.isDir {
				ext := strings.ToLower(strings.TrimPrefix(path.Ext(file.name), "."))
				if _, ok := extMap[ext]; ok {
					file.WithCompression(Store, 0)
				}
			}
			return true
		})
	}
}

// SecuritySettings define extraction safety configuration.
type SecuritySettings struct {
	// AllowSymlinks enables extraction of symbolic links.
	// WARNING: disabling this is recommended for untrusted archives.
	// Default: false.
	AllowSymlinks bool

	// MinEncryption allows enforcing a minimum encryption standard.
	// e.g. Require AES256 to prevent downgrade attacks.
	MinEncryption EncryptionMethod

	// ResourceLimits defines constraints for extraction.
	ResourceLimits ResourceLimits
}

// ResourceLimits defines constraints for extraction.
type ResourceLimits struct {
	// MaxTotalSize is the maximum allowed bytes to write to disk for the whole operation.
	// Default: 0 (unlimited).
	MaxTotalSize int64

	// MaxFileSize is the maximum allowed size for a single file.
	// Default: 0 (unlimited).
	MaxFileSize int64

	// MaxCompressionRatio is the maximum allowed ratio between uncompressed and compressed size.
	// E.g., 100 means uncompressed data cannot be more than 100x larger than compressed.
	// Default: 0 (disabled). Recommended: 100-200.
	MaxRatio float64
}

// WithSecurity applies security settings for extraction.
func WithSecurity(settings SecuritySettings) ZipOption {
	return func(pc *processConfig) {
		pc.security = settings
	}
}

// ProgressStats contains detailed information about the current progress of the operation.
type ProgressStats struct {
	CurrentFile    *File // Currently processed file
	CurrentRead    int64 // Uncompressed bytes read from current file
	CurrentWritten int64 // Bytes produced for current file
	ExpectedRead   int64 // Sum of uncompressed file sizes
	TotalRead      int64 // Total uncompressed bytes read so far
	TotalWritten   int64 // Total compressed bytes produced so far
	ArchiveWritten int64 // Total bytes written to dest (headers + data + CD)
	TotalFiles     int64 // Total amount of files to process
	ProcessedFiles int64 // The total number of files processed, including errors.
	Errors         int64 // Failed files count
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
		atomic.StoreInt64(&c.stats.ArchiveWritten, c.destCounter.Count())
	}
	c.notify(f)
}

// OnWritten handles updates when bytes are written.
func (c *statsCollector) OnWritten(_ *File, n int, written int64) {
	if c.stats == nil {
		return
	}
	atomic.AddInt64(&c.stats.TotalWritten, int64(n))
	atomic.StoreInt64(&c.stats.CurrentWritten, written)
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
		CurrentFile:    current,
		CurrentRead:    atomic.LoadInt64(&c.stats.CurrentRead),
		CurrentWritten: atomic.LoadInt64(&c.stats.CurrentWritten),
		TotalRead:      atomic.LoadInt64(&c.stats.TotalRead),
		TotalWritten:   atomic.LoadInt64(&c.stats.TotalWritten),
		ProcessedFiles: atomic.LoadInt64(&c.stats.ProcessedFiles),
		Errors:         atomic.LoadInt64(&c.stats.Errors),
		ExpectedRead:   c.stats.ExpectedRead, // const
		TotalFiles:     c.stats.TotalFiles,   // const
		ArchiveWritten: atomic.LoadInt64(&c.stats.ArchiveWritten),
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

// Default limits to prevent denial of service
const (
	defaultGraceSpace  = 10 * 1024 * 1024 // 10 MB grace period
	defaultRatioBuffer = 4096             // Buffer to smooth out ratio calc for small files
)

type secureWriter struct {
	w io.Writer

	// Counters
	written      int64  // Bytes written for current file
	totalWritten *int64 // Pointer to global atomic counter

	maxFileSize  int64
	maxTotalSize int64
	maxRatio     float64

	// Ratio calculation data
	compressedSize int64 // From file header
}

func (sw *secureWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	writeLen := int64(n)

	if sw.maxTotalSize > 0 {
		newTotal := atomic.AddInt64(sw.totalWritten, writeLen)
		if newTotal > sw.maxTotalSize {
			atomic.AddInt64(sw.totalWritten, -writeLen)
			return 0, fmt.Errorf("%w: global limit %d bytes exceeded", ErrResourceLimit, sw.maxTotalSize)
		}
	}

	if sw.maxFileSize > 0 {
		if sw.written+writeLen > sw.maxFileSize {
			return 0, fmt.Errorf("%w: file limit %d bytes exceeded", ErrResourceLimit, sw.maxFileSize)
		}
	}

	if sw.maxRatio > 0 && (sw.written+writeLen) > defaultGraceSpace {

		// Formula: Written / (Compressed + Buffer)
		// Buffer prevents division by zero and false positives on tiny files
		denominator := float64(sw.compressedSize + defaultRatioBuffer)
		currentRatio := float64(sw.written+writeLen) / denominator

		if currentRatio > sw.maxRatio {
			return 0, fmt.Errorf("%w: compression ratio %.2fx exceeds limit %.2fx",
				ErrResourceLimit, currentRatio, sw.maxRatio)
		}
	}

	n, err = sw.w.Write(p)
	sw.written += int64(n)
	return n, err
}
