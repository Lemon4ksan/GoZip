# GoZip

[![Go Reference](https://pkg.go.dev/badge/github.com/lemon4ksan/gozip.svg)](https://pkg.go.dev/github.com/lemon4ksan/gozip)
[![Go Report Card](https://goreportcard.com/badge/github.com/lemon4ksan/gozip)](https://goreportcard.com/report/github.com/lemon4ksan/gozip)

**GoZip** is a high-performance, feature-rich library for creating, reading, modifying, and extracting ZIP archives in Go. It is written in pure Go without CGO or external dependencies.

Designed for high-load applications, GoZip focuses on **concurrency**, **memory safety**, **strict standard compliance**, and **developer experience**, fixing common pain points found in the standard library (like legacy encodings, Zip64 limits, and WinZip AES compatibility).

## ⚡ Performance Benchmarks

GoZip achieves performance parity with the standard library (`archive/zip`) in sequential mode while offering significant speedups in parallel mode by utilizing all available CPU cores.

| Scenario | Standard Lib | GoZip (Sequential) | GoZip (Parallel)
| :--- | :--- | :--- | :---
| **Write 1000 Small Files** | 14.77 ms | 14.81 ms | **6.32 ms (2.3x faster)**
| **Write 10 Medium Files (100MB)** | 175.6 ms | 176.3 ms | **42.5 ms (4.1x faster)**
| **Metadata Parsing** | 0.12 ms | 3.06 ms | **0.27 ms (StreamReader)**

*Benchmarks run on **Intel Core i5-12400F** (6 cores, 12 threads).*

## 🚀 Key Features

* **High-Level Helpers:** `Archiver` abstraction over `Zip` reduces boilerplate code significantly.
* **Archiver Pattern:** Encapsulated configuration (passwords, codecs) without global state side-effects.
* **Parallel Processing:** Built-in worker pools for compression and extraction.
* **Smart I/O:** Automatically switches between stream processing and temporary file buffering based on file size and capabilities.
* **Security:** Native **Zip Slip** protection and **AES-256** (WinZip compatible) encryption.
* **Context Support:** Full cancellation and timeout support for all long-running operations.
* **Edit Capability:** Modify existing archives (Rename, Move, Remove) with structural safety.
* **Legacy Support:** Handles **Zip64**, **NTFS timestamps**, and **CP866 (DOS)** encoding automatically.

## 📦 Installation

```bash
go get github.com/lemon4ksan/gozip@latest
```

## 📖 Quick Start

For 90% of use cases, use the high-level static functions. They use safe defaults (Deflate compression, auto-detection).

```go
package main

import "github.com/lemon4ksan/gozip"

func main() {
    // Automatically walks the folder and compresses files using all CPU cores
    gozip.ArchiveDir(
        "data/images",
        gozip.ToFilePath("images_backup.zip"),
        gozip.WithWorkers(runtime.NumCPU()),
    )

    // Extract specific files from an archive
    gozip.Unzip(
        gozip.FromFilePath("images_backup.zip"),
        "restored_images/",
        gozip.WithExcludeDir("2020"),
    )

    // Read a single config file content directly into memory
    configBytes, _ := gozip.ReadFile(
        gozip.FromFilePath("app_data.zip"),
        "config.json",
    )
}
```

## 🛠 Advanced Usage

### 1. The Archiver (Custom Configuration)

Use `NewArchiver` to create a configured environment. This is ideal for dependency injection or when you need specific settings (like encryption or custom codecs) isolated from other parts of your app.

```go
func main() {
    // Create an archiver with specific settings
    archiver := gozip.NewArchiver(
        gozip.WithArchivePassword("secure-password-123"),
        gozip.WithCompression(gozip.ZStandard, zstd.SpeedBestCompression),
        zstd.Enable(),
    )

    // Use this instance to perform operations
    err := archiver.Unzip(
        gozip.FromFilePath("encrypted_data.zip"), 
        "output_dir/",
    )
    if err != nil {
        panic(err)
    }
}
```

### 2. Low-Level Control & Modification

For granular control (e.g., adding files lazily, modifying existing archives), use the `NewZip` directly.

```go
func main() {
    archive := gozip.NewZip()

    // Open existing archive for editing
    f, _ := os.Open("backup.zip")
    defer f.Close()
    archive.LoadFromFile(f)

    // Modify structure
    archive.Remove("old_logs/")
    archive.Rename("config.yaml", "config.old.yaml")
    
    // Add dynamic content (Lazy)
    // The function is called only when WriteTo is executed
    archive.AddLazy("db_dump.sql", func() (io.ReadCloser, error) {
        return exec.Command("pg_dump", "db").StdoutPipe()
    })

    // Save changes
    out, _ := os.Create("backup_v2.zip")
    archive.WriteTo(out)
}
```

### 3. Context Support (Timeouts)

Safe extraction with timeout protection using `...WithContext` methods.

```go
func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // If extraction takes longer than 30s, it cancels automatically
    // and cleans up partially extracted files.
    err := gozip.UnzipWithContext(ctx, gozip.FromFilePath("huge.zip"), "output/")
}
```

### 4. Encryption (AES-256) 🔒

GoZip supports strong encryption compatible with WinZip and 7-Zip.

```go
func main() {
    // Create an encrypted archive
    cfg := gozip.ZipConfig{
        EncryptionMethod: gozip.AES256,
        Password:         "MySecret",
    }
    archive := gozip.NewZip().SetConfig(cfg)

    archive.AddFile("secrets.txt")
    
    out, _ := os.Create("secure.zip")
    archive.WriteTo(out)

    // Decrypt using helper
    gozip.Unzip(
        gozip.FromFilePath("secure.zip"), 
        "out_dir/", 
        gozip.WithOpPassword("MySecret"),
    )
}
```

## 🛠 Utilities & I/O Abstraction

GoZip exposes its internal resource management logic via `UseSource` and `UseSink`.
You can use the library's `Source` abstractions (File, URL, Stream, Buffer) for your own tasks, like hashing, signing, or uploading data, without writing boilerplate for opening/closing resources or handling `io.Reader` vs `io.ReaderAt`.

### Example: Calculate SHA-256 hash of any source (File, URL, or Memory)

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "github.com/lemon4ksan/gozip"
)

func CalculateHash(src gozip.Source) (string, error) {
    var hash string
    
    // Automatically handles Open/Close, and provides both
    // Reader (stream) and ReaderAt (random access) interfaces.
    err := gozip.DefaultArchiver.UseSource(src, 
        func(r io.Reader, rAt io.ReaderAt, size int64) error {
            h := sha256.New()
            
            // Just read from the stream. 
            // If it's a URL, it streams. If it's a file, it reads from disk.
            if _, err := io.Copy(h, r); err != nil {
                return err
            }
            
            hash = hex.EncodeToString(h.Sum(nil))
            return nil
        },
    )
    
    return hash, err
}

func main() {
    // Works uniformly!
    h1, _ := CalculateHash(gozip.FromFilePath("local.iso"))
    h2, _ := CalculateHash(gozip.FromURL("https://example.com/image.iso", nil))
}
```

## 🌊 Streaming Reader (Sequential Access)

While the `Archiver` and `Zip` act as random-access managers, `StreamReader` allows processing archives sequentially. This is ideal for reading ZIP files directly from **HTTP response bodies** or **pipes** where seeking is impossible.

```go
func main() {
    resp, _ := http.Get("https://example.com/data.zip")
    defer resp.Body.Close()

    // Read sequentially without downloading the whole file
    sr := gozip.NewStreamReader(resp.Body)

    for {
        f, err := sr.Next()
        if err == io.EOF { break }

        if f.Name() == "target.txt" {
            rc, _ := sr.Open()
            data, _ := io.ReadAll(rc)
            fmt.Printf("Content: %s\n", string(data))
            rc.Close()
        }
    }
}
```

## ⚠️ Error Handling

GoZip uses structured error handling. Bulk operations return `errors.Join`, and specific file errors are wrapped in `*FileError`.

```go
if err := archive.AddFile("data/report.pdf"); err != nil {
    var fileErr *gozip.FileError
    if errors.As(err, &fileErr) {
        fmt.Printf("Operation: %s\n", fileErr.Op)   // e.g., "add", "stat", "open"
        fmt.Printf("File:      %s\n", fileErr.File.Name())
        fmt.Printf("Cause:     %v\n", fileErr.Err)  // Underlying error (e.g., os.ErrPermission)
    }
}
```

### Errors Reference

| Error | Description
| :--- | :---
| `ErrFormat` | Not a valid ZIP archive (invalid signatures).
| `ErrPasswordMismatch` | Incorrect password or missing password.
| `ErrChecksum` | CRC-32 integrity check failed.
| `ErrInsecurePath` | **Zip Slip** detected: file path attempts to escape destination.
| `ErrDuplicateEntry` | A file with this name already exists in the archive.
| `ErrAlgorithm` | Compression method not supported (e.g., LZMA without plugin).
| `ErrFileNotFound` | Requested entry is missing. Wraps `fs.ErrNotExist`.
| `ErrFilenameTooLong` | Filename exceeds the ZIP limit of 65,535 bytes.
| `ErrResourceLimit` | Extraction exceeded defined limits.
| `ErrNotImplemented` | Code path is not implemented.

## ⚙️ Configuration & Options

### Functional Options

Configure operations per-file or per-archive:

* `WithName("new.txt")`: Rename file inside the archive.
* `WithCompression(method, level)`: Override compression for specific files.
* `WithEncryption(method, password)`: Override encryption.
* `WithWorkers(n)`: Set number of parallel workers.
* `WithProgress(callback)`: track progress of operations.

### Filters

Select which files to process:

* `WithOnly([]string)`: Process only specific files.
* `WithFromDir("folder")`: Restrict to a directory.
* `WithExcludeDir("folder")`: Exclude a directory.

### Sort Strategies

* `SortDefault`: Preserves insertion order.
* `SortAlphabetical`: Sorts by name (A-Z).
* `SortSizeDescending`: Optimizes parallel writing.
* `SortZIP64Optimized`: Buckets files by size to optimize Zip64 header overhead.

To optimize memory usage and avoid high heap peaks, use `SortSizeDescending`.
This ensures large buffers are reused efficiently and heavy files don't block the output queue.

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
