# GoZip

[![Go Reference](https://pkg.go.dev/badge/github.com/lemon4ksan/gozip.svg)](https://pkg.go.dev/github.com/lemon4ksan/gozip)
[![Go Report Card](https://goreportcard.com/badge/github.com/lemon4ksan/gozip)](https://goreportcard.com/report/github.com/lemon4ksan/gozip)

**A high-performance, concurrency-safe replacement for the standard `archive/zip`.**

GoZip is designed for high-load applications where speed, security, and interface flexibility are critical. It treats file systems, memory buffers, and network streams as first-class citizens, allowing you to manipulate archives without managing temporary files or complex buffers.

## ⚡ At a Glance

Why switch from the standard library?

| Feature | `archive/zip` (StdLib) | **GoZip** |
| :--- | :--- | :--- |
| **Concurrency** | Single-threaded | **Parallel (Multi-core)** |
| **Performance** | Baseline | **~4x Faster** (See Benchmarks) |
| **Security** | Vulnerable to Zip Slip | **Native Protection** |
| **I/O Source** | File / ReaderAt | **Polymorphic** (File, URL, Buffer) |
| **Encryption** | Legacy ZipCrypto (Read-only) | **AES-256** & ZipCrypto (R/W) |
| **Editing** | Append only | **Modify / Rename / Remove** |
| **Control** | No cancellation | **Context-aware** |

## 🚀 Key Capabilities

* **Cloud-Native I/O:** Stream archives directly from HTTP/S3 to the client without touching the disk.
* **No External Dependencies** The core library relies strictly on the Go standard library.
* **Zero-Overhead Abstraction:** Unified API for files, bytes, and streams using `Source` and `Sink` interfaces.
* **The Archiver Pattern:** No global state side-effects. Configure passwords and codecs once, reuse everywhere.
* **Resilience:** "Best Effort" strategy collects all errors (`errors.Join`) instead of failing on the first file.
* **Legacy Support:** Automatically handles Zip64, NTFS timestamps, Unix permissions, and CP866 (DOS) encodings.

---

## 📊 Performance Benchmarks

GoZip utilizes a fixed worker pool, memory pooling (`sync.Pool`), and a Zero-Allocation pipeline. It achieves massive speedups while maintaining **allocation parity** with the standard library.

> **Environment:** Intel Core i5-12400F (6 cores, 12 threads)

### Compression Speed

| Scenario | Standard Lib | GoZip (Sequential) | GoZip (Parallel) | Speedup |
| :--- | :--- | :--- | :--- | :--- |
| **1,000 Small Files** | 14.3 ms | 15.0 ms | **3.9 ms** | **3.7x faster** |
| **10 Medium Files (100MB)** | 179.8 ms | 177.9 ms | **42.2 ms** | **4.3x faster** |

### Memory Efficiency

| Metric | Standard Lib | GoZip (Sequential) | GoZip (Parallel) | Impact |
| :--- | :--- | :--- | :--- | :--- |
| **Allocations** | 12,016 op | 12,078 op | **12,568 op** | **< 5% overhead** |
| **Memory Usage** | 0.48 MB/op | 1.71 MB/op | 13.8 MB/op | Bounded by worker count |

*Note: GoZip trades a fixed amount of buffer memory (per worker) to saturate CPU cores, ensuring the GC remains idle even under heavy load.*

---

## 📦 Installation

```bash
go get github.com/lemon4ksan/gozip@latest
```

## 📖 Usage Guide

### 1. The One-Liners (Simple)

For 90% of use cases, use the static helpers with safe defaults (Deflate compression, auto-detection).

```go
package main

import "github.com/lemon4ksan/gozip"

func main() {
    // Archive a directory to a file
    gozip.ArchiveDir("data/images", gozip.ToFilePath("backup.zip"))

    // Extract specific files
    gozip.Unzip(
        gozip.FromFilePath("backup.zip"),
        "restored/",
        gozip.WithFromDir("images"), // Extract only this folder
    )

    // Read single file content directly into memory
    data, _ := gozip.ReadFile(gozip.FromFilePath("logs.zip"), "error.log")
}
```

### 2. The Archiver (Configured)

Use `NewArchiver` when you need specific settings (encryption, compression levels) isolated from the rest of your app.

```go
func main() {
    // Create a reusable configuration
    archiver := gozip.NewArchiver(
        gozip.WithArchivePassword("secure-password-123"),
        gozip.WithCompression(gozip.ZStandard, zstd.SpeedBestCompression),
        // gozip.WithWorkers(4), // Optional: Limit concurrency
    )

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Use it for multiple operations
    err := archiver.UnzipWithContext(ctx, gozip.FromFilePath("encrypted.zip"), "out/")
}
```

### 3. Modifying Archives (Edit Mode)

GoZip allows surgical modifications to existing archives (Edit-in-place logic).

```go
func main() {
    archive := gozip.NewZip()
    
    // Load existing metadata (lightweight, doesn't read contents yet)
    src, _ := os.Open("app.zip")
    archive.LoadFromFile(src)
    src.Close()

    // Modify structure
    archive.Remove("logs/")
    archive.Rename("config.dev.json", "config.json")
    
    // Add new file dynamically (Lazy execution)
    archive.AddLazy("db.dump", func() (io.ReadCloser, error) {
        return exec.Command("pg_dump", "db").StdoutPipe()
    })

    // Write the new version
    out, _ := os.Create("app_v2.zip")
    archive.WriteTo(out)
}
```

### 4. Advanced: Polymorphic I/O

You can perform operations on any `Source` (File, URL, Buffer) using the unified API.

```go
// Calculate SHA256 of a remote ZIP without storing it on disk
func HashRemoteZip(url string) string {
    h := sha256.New()
    
    // gozip handles the HTTP range requests or buffering automatically
    gozip.UseSource(gozip.FromURL(url, http.DefaultClient), 
        func(r io.Reader, _ io.ReaderAt, _ int64) error {
            _, err := io.Copy(h, r)
            return err
        },
    )
    return hex.EncodeToString(h.Sum(nil))
}
```

---

## ⚙️ Configuration & Options

### Functional Options

Configure operations per-file or per-archive:

* `WithName("new.txt")`: Rename file inside the archive.
* `WithCompression(method, level)`: Override compression.
* `WithEncryption(method, password)`: Set AES-256 or ZipCrypto.
* `WithWorkers(n)`: Set number of parallel workers.
* `WithProgress(callback)`: track progress of operations.

### Filters

Selectively process files:

* `WithOnly([]*File)`: Whitelist specific files.
* `WithFromDir("folder")`: Restrict operation to a specific directory.
* `WithExcludeDir("folder")`: Blacklist a directory.

### Sorting Strategies

* `SortDefault`: Preserves insertion order.
* `SortAlphabetical`: Deterministic order (A-Z).
* `SortSizeDescending`: Ensures heavy files don't block the output queue in parallel mode.
* `SortZIP64Optimized`: Buckets files by size to optimize Zip64 header overhead.

## ⚠️ Error Handling

GoZip uses structured error handling.

* **Bulk operations** return `errors.Join`.
* **Specific errors** are wrapped in `*FileError` for introspection.

| Error | Description |
| :--- | :--- |
| `ErrFormat` | Not a valid ZIP archive (invalid signatures). |
| `ErrPasswordMismatch` | Incorrect password or missing password. |
| `ErrChecksum` | CRC-32 integrity check failed. |
| `ErrInsecurePath` | **Zip Slip** detected: file path attempts to escape destination. |
| `ErrDuplicateEntry` | A file with this name already exists in the archive. |
| `ErrAlgorithm` | Compression method not supported (e.g., LZMA without plugin). |
| `ErrFileNotFound` | Requested entry is missing. Wraps `fs.ErrNotExist`. |
| `ErrFilenameTooLong` | Filename exceeds the ZIP limit of 65,535 bytes. |
| `ErrResourceLimit` | Extraction exceeded defined limits. |
| `ErrNotImplemented` | Code path is not implemented. |

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
