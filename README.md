# GoZip

[![Go Reference](https://pkg.go.dev/badge/github.com/lemon4ksan/gozip.svg)](https://pkg.go.dev/github.com/lemon4ksan/gozip)
[![Go Report Card](https://goreportcard.com/badge/github.com/lemon4ksan/gozip)](https://goreportcard.com/report/github.com/lemon4ksan/gozip)

**GoZip** is a high-performance, feature-rich library for creating, reading, modifying, and extracting ZIP archives in Go. It is written in pure Go without CGO or external dependencies.

Designed for high-load applications, GoZip focuses on **concurrency**, **memory safety**, and **strict standard compliance**, fixing common pain points found in the standard library (like legacy encodings, Zip64 limits, and WinZip AES compatibility).

## ⚡ Performance Benchmarks

GoZip achieves performance parity with the standard library in sequential mode while offering near-linear scalability in parallel mode.

| Scenario | Standard Lib | GoZip (Sequential) | GoZip (Parallel 12 workers) |
| :--- | :--- | :--- | :--- |
| **1000 Small Files (1KB)** | 20 ms | 20 ms | **7.3 ms (2.8x faster)** |
| **10 Medium Files (10MB)** | 1.60 s | 1.57 s | **0.25 s (6.4x faster)** |

*Benchmarks run on **Intel Core i5-12400F** (6 cores, 12 threads).*

You can speed up the time even further by registering a faster flate implementation. ([`github.com/klauspost/compress/flate`](https://github.com/klauspost/compress) for example)

## 🚀 Key Features

* **High Performance:** Built-in support for **parallel compression and extraction** using worker pools.
* **Concurrency Safe:** Optimized for concurrent access using `io.ReaderAt`, allowing wait-free parallel reading.
* **Smart I/O:** Automatically switches between stream processing and temporary file buffering based on file size and capabilities.
* **Archive Modification:** Supports renaming, moving, and removing files/directories within an existing archive.
* **Developer Experience:** Helpers for common tasks: `AddString`, `AddBytes`, `AddLazy`, `Find`, `Glob`, `LoadFromFile`.
* **Context Support:** Full support for `context.Context` (cancellation/timeouts) for all long-running operations.
* **Security:**
  * **Zip Slip** protection during extraction.
  * **AES-256** (WinZip compatible) and legacy **ZipCrypto** encryption support.
* **Cross-Platform Metadata:** Preserves **NTFS** (Windows) timestamps and **Unix/macOS** file permissions.
* **Legacy Compatibility:** Includes support for **CP866 (Cyrillic DOS)** and **CP437** encodings.

## 📦 Installation

```bash
go get github.com/lemon4ksan/gozip
```

## 📖 Usage Examples

### 1. Creating an Archive

The simplest way to create an archive. `AddFile` is lazy and efficient.

```go
package main

import (
    "os"
    "github.com/lemon4ksan/gozip"
)

func main() {
    archive := gozip.NewZip()

    // Add a single file from disk
    archive.AddFile("document.txt")

    // Add data directly from memory
    archive.AddString("debug mode=on", "config.ini")
    archive.AddBytes([]byte{0xDE, 0xAD, 0xBE, 0xEF}, "bin/header.bin")

    // Add a directory recursively
    // You can override compression per file
    archive.AddDir("images", gozip.WithCompression(gozip.Deflated, gozip.DeflateMaximum))

    out, _ := os.Create("backup.zip")
    defer out.Close()

    // Write sequentially to the output file
    if _, err := archive.WriteTo(out); err != nil {
        panic(err)
    }
}
```

### 2. Parallel Archiving (High Speed) ⚡

Use `WriteToParallel` to utilize multiple CPU cores.

```go
func main() {
    archive := gozip.NewZip()
    archive.AddDir("huge_dataset")

    out, _ := os.Create("data.zip")
    defer out.Close()

    // Use all available CPU cores
    _, err := archive.WriteToParallel(out, runtime.NumCPU())
    if err != nil {
        panic(err)
    }
}
```

### 3. Modifying an Archive (Edit Mode)

GoZip allows you to load an existing archive, modify its structure, and save it.

```go
func main() {
    archive := gozip.NewZip()

    // Open existing archive
    f, _ := os.Open("backup.zip")
    defer f.Close()

    // Parse structure
    if err := archive.LoadFromFile(f); err != nil {
        panic(err)
    }

    // 1. Remove files
    archive.Remove("secret_config.yaml")
    archive.Remove("temp_cache") // Recursive removal

    // 2. Rename/Move files
    if file, err := archive.File("images/old_logo.png"); err == nil {
        archive.Move(file.Name(), "assets/graphics")
        archive.Rename(file.Name(), "new_logo.png")
    }

    // 3. Modify a file
    file := archive.File("data/config.json")
    archive.Remove(file.Name())
    archive.AddLazy(file.Name(), func() (io.ReadCloser, error) {
        rc, err := file.Open()
        if err != nil {
            return nil, err
        }
        defer rc.Close()

        // Modify original data ...

        return io.NopCloser(bytes.NewReader(processedData)), nil
    })

    // 4. Add new content
    archive.AddString("Updated at 2025", "meta.txt")

    // Save changes to a new file
    out, _ := os.Create("backup_v2.zip")
    defer out.Close()

    // Zero-copy optimization: unaltered files are copied directly without re-compression
    archive.WriteTo(out)
}
```

### 4. Extracting Files with Context (Timeout)

Safe extraction with timeout protection.

```go
func main() {
    archive := gozip.NewZip()
    f, _ := os.Open("huge_backup.zip")
    defer f.Close()
    archive.LoadFromFile(f)

    // Create a context with a 30-second timeout
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Extract files concurrently
    // If it takes longer than 30s, it cancels automatically and cleans up
    err := archive.ExtractParallelWithContext(ctx, "output_dir", 8)
    if err != nil {
        if errors.Is(err, context.DeadlineExceeded) {
            fmt.Println("Extraction timed out!")
        }
    }
}
```

### 5. Virtual File Systems 📂

Work with files abstractly, without relying on physical disk.

```go
package main

import (
    "embed"
    "github.com/lemon4ksan/gozip"
)

//go:embed templates/* static/*
var assets embed.FS

func main() {
    archive := gozip.NewZip()

    // Recursively add embed.FS
    if err := archive.AddFS(assets); err != nil {
        panic(err)
    }

    // Turn archive into file system
    fileSystem := archive.FS()

    // Read files with fs interface
    data, _ := fs.ReadFile(fileSystem, "style.css")

    // Use in HTTP
    http.Handle("/", http.FileServer(http.FS(fileSystem)))

    http.ListenAndServe(":8080", nil)
}
```

### 6. Encryption (AES-256) 🔒

GoZip supports strong encryption compatible with WinZip and 7-Zip.

```go
func main() {
    archive := gozip.NewZip()

    // Set global configuration
    archive.SetConfig(gozip.ZipConfig{
        CompressionMethod: gozip.Deflated,
        CompressionLevel:  gozip.DeflateNormal,
        EncryptionMethod:  gozip.AES256, // Recommended
        Password:          "MySecretPassword123",
    })

    archive.AddFile("secret.pdf")

    out, _ := os.Create("secure.zip")
    archive.WriteTo(out)
}
```

### 7. Fixing Broken Encodings (CP866 / Russian DOS)

Read archives created on old Windows systems that appear as gibberish (e.g., `ΓÑßΓ.txt`).

```go
func main() {
    archive := gozip.NewZip()

    // Configure fallback encoding
    archive.SetConfig(gozip.ZipConfig{
        TextEncoding: gozip.DecodeIBM866, // Fixes Cyrillic CP866
    })

    f, _ := os.Open("old_dos_archive.zip")
    archive.LoadFromFile(f)

    // Filenames are now correctly converted to UTF-8
    archive.Extract("output")
}
```

## ⚠️ Error Handling

GoZip uses typed sentinel errors:

```go
if err := archive.Extract("out"); err != nil {
    if errors.Is(err, gozip.ErrPasswordMismatch) {
        // Prompt user for password again
    } else if errors.Is(err, gozip.ErrFormat) {
        // Not a valid zip file
    } else if errors.Is(err, gozip.ErrFilenameTooLong) {
        // Handle limitation
    }
}
```

**Available Errors:**

* `ErrFormat`, `ErrAlgorithm`, `ErrEncryption`
* `ErrPasswordMismatch`, `ErrChecksum`, `ErrSizeMismatch`
* `ErrFileNotFound`, `ErrDuplicateEntry`
* `ErrInsecurePath` (Zip Slip attempt)
* `ErrFilenameTooLong`, `ErrCommentTooLong`, `ErrExtraFieldTooLong`

## ⚙️ Configuration & Options

### Functional Options

Configure individual files using the Option pattern:

* `WithName("new_name.txt")`: Rename file inside the archive.
* `WithPath("folder/subfolder")`: Place file inside a specific virtual path.
* `WithCompression(method, level)`: Override compression for this file.
* `WithEncryption(method, password)`: Override encryption for this file.
* `WithMode(0755)`: Set custom file permissions (Unix style).

### Sort Strategies

* `SortDefault`: Preserves insertion order.
* `SortAlphabetical`: Sorts by name (A-Z).
* `SortSizeDescending`: Optimizes parallel writing.
* `SortZIP64Optimized`: Buckets files by size to optimize Zip64 header overhead.

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
