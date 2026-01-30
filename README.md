# GoZip

[![Go Reference](https://pkg.go.dev/badge/github.com/lemon4ksan/gozip.svg)](https://pkg.go.dev/github.com/lemon4ksan/gozip)
[![Go Report Card](https://goreportcard.com/badge/github.com/lemon4ksan/gozip)](https://goreportcard.com/report/github.com/lemon4ksan/gozip)

**GoZip** is a high-performance, feature-rich library for creating, reading, modifying, and extracting ZIP archives in Go. It is written in pure Go without CGO or external dependencies.

Designed for high-load applications, GoZip focuses on **concurrency**, **memory safety**, and **strict standard compliance**, fixing common pain points found in the standard library (like legacy encodings, Zip64 limits, and WinZip AES compatibility).

## ⚡ Performance Benchmarks

GoZip achieves performance parity with the standard library in sequential mode while offering near-linear scalability in parallel mode.

| Scenario | Standard Lib | GoZip (Sequential) | GoZip (Parallel 12 workers) |
| :--- | :--- | :--- | :--- |
| **Write 1000 Small Files** | 57.4 ms | 55.4 ms | **9.7 ms (5.9x faster)** |
| **Write 10 Medium Files (100MB)** | 1.23 s | 1.21 s | **0.20 s (6.1x faster)** |
| **Metadata Parsing (1000 files)** | 0.12 ms | 3.04 ms | **0.25 ms (StreamReader)** |

*Benchmarks run on **Intel Core i5-12400F** (6 cores, 12 threads).*

GoZip's `Load` is slower than stdLib because it eagerly builds an O(1) lookup map and ensures structural safety. Use **StreamReader** for maximum efficiency during sequential processing.

### StreamReader Efficiency

If you don't need random access to files, `StreamReader` is the fastest way to process an archive. It skips the expensive index-building step:

* **12x faster** initial access compared to `archive.Load()`.
* **Minimal memory footprint** as it only keeps one file header in memory at a time.
* Ideal for high-throughput data pipelines and cloud functions.

## Custom algorithms

You can speed up the time even further by registering a faster flate implementation. ([`github.com/klauspost/compress/flate`](https://github.com/klauspost/compress) for example)

```go
// Example registration:
archive.RegisterCompressor(gozip.Deflate, func(level int) gozip.Compressor {
    return klauspost_wrapper.New(level)
})
```

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
go get github.com/lemon4ksan/gozip@latest
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
    archive.AddDir("images", gozip.WithCompression(gozip.Deflate, gozip.DeflateMaximum))

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
    if file, ok := archive.File("images/old_logo.png"); ok {
        archive.Move(file.Name(), "assets/graphics")
        archive.Rename(file.Name(), "new_logo.png")
    }

    // 3. Modify a file
    file, _ := archive.File("data/config.json")
    archive.Remove(file.Name())
    archive.AddLazy(file.Name(), func() (io.ReadCloser, error) {
        pr, pw := io.Pipe()

        go func() {
            defer pw.Close() 

            rc, err := file.Open()
            if err != nil {
                pw.CloseWithError(err)
                return
            }
            defer rc.Close()

            processor.Transform(rc, pw)
        }()

        return pr, nil
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
        CompressionMethod: gozip.Deflate,
        CompressionLevel:  gozip.DeflateNormal,
        EncryptionMethod:  gozip.AES256, // Recommended
        Password:          "MySecretPassword123",
    })

    archive.AddFile("secret.pdf")

    out, _ := os.Create("secure.zip")
    archive.WriteTo(out)
}
```

If you want to remove encryption, you can do it as follows:

```go
func main() {
    archive := gozip.NewZip()
    // Register compressors & decompressors if needed

    archive.SetConfig(gozip.ZipConfig{
        Password: "pass",
    })

    f, _ := os.Open("encrypted.zip")
    defer f.Close()

    // Only the password will apply
    if err := archive.LoadFromFile(f); err != nil {
        panic(err)
    }

    // Remove encryption and set new compression level for each file
    for _, file := range archive.Files() {
        // To replace initial password use file.SetSourcePassword()
        // in case if it's incorrect
        file.DisableEncryption()
        file.SetCompression(gozip.Deflate, gozip.DeflateMaximum)
    }

    out, _ := os.Create("output.zip")
    defer out.Close()

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    _, err := archive.WriteToParallelWithContext(ctx, out, runtime.NumCPU())
    if err != nil {
        if errors.Is(err, context.Canceled) {
            out.Close()
            os.Remove("output.zip")
            return
        }
        panic(err)
    }
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

## 🌊 Streaming Reader (Sequential Access)

While the standard `Zip` object requires random access (`io.ReaderAt`), GoZip provides a `StreamReader` for processing archives sequentially. This is ideal for reading ZIP files directly from **HTTP response bodies**, **TCP connections**, or **Unix pipes** without saving them to disk.

### Key Advantages

* **Memory Efficient:** Processes files one by one with a tiny memory footprint.
* **No Seek Required:** Works with any `io.Reader`.
* **Data Descriptor Support:** Correctly handles archives created in streaming mode (where file sizes are unknown until the end of the file data).

### Example: Processing a remote ZIP via HTTP

```go
package main

import (
    "io"
    "net/http"
    "github.com/lemon4ksan/gozip"
)

func main() {
    resp, err := http.Get("https://example.com/huge_backup.zip")
    if err != nil {
        panic(err)
    }
    defer resp.Body.Close()

    // Initialize StreamReader from the network stream
    sr := gozip.NewStreamReader(resp.Body)

    for {
        // Move to the next file in the stream
        f, err := sr.Next()
        if err == io.EOF {
            break // End of archive
        }
        if err != nil {
            panic(err)
        }

        // Process only specific files without downloading the rest
        if isRequired(f.Name()) {
            rc, _ := sr.Open()

            // Integrity check happens inside io.ReadAll at the very end.
            data, err := io.ReadAll(rc)
            if err != nil {
                if errors.Is(err, gozip.ErrChecksum) {
                    fmt.Println("Error: File is corrupted")
                }
            }

            rc.Close()
        }
        // sr.Next() will automatically skip remaining data of the current file
    }
}
```

### Limitations of Streaming Mode

Due to the nature of the ZIP format, reading sequentially has some trade-offs:

1. **Limited Metadata:** Since `StreamReader` reads Local File Headers instead of the Central Directory at the end, some attributes (like Unix permissions, file comments, or precise NTFS timestamps) are unavailable.
2. **No Backtracking:** Once a file is skipped or read, you cannot go back to it without restarting the entire stream.
3. **Data Descriptor Scanning:** For `Stored` (uncompressed) files with unknown sizes, the reader must scan the stream for signatures, which has a very small chance of false positives in purely random binary data.

## ⚠️ Error Handling

GoZip provides a structured error system. Instead of simple strings, most operations return errors that can be inspected to find exactly which file caused the issue and why.

### 1. The `FileError` Structure

Whenever an error is tied to a specific archive entry, GoZip wraps it in a `*FileError`.

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

### 2. Handling Bulk Operations (`errors.Join`)

Methods that process multiple files (like `AddDir`, `Extract`, or `WriteTo`) use a "Best Effort" strategy.
They continue processing after non-fatal errors and return a combined error using `errors.Join`.

To inspect all errors in a combined result:

```go
if err := archive.Extract("./out"); err != nil {
    // Standard way to unwrap joined errors (Go 1.20+)
    if e, ok := err.(interface{ Unwrap() []error }); ok {
        for _, subErr := range e.Unwrap() {
            var fErr *gozip.FileError
            if errors.As(subErr, &fErr) {
                log.Printf("Failed to extract %s: %v", fErr.File.Name(), fErr.Err)
            }
        }
    }
}
```

### 3. Sentinel Errors Reference

### Error Reference

| Error | Description |
| :--- | :--- |
| `ErrFormat` | Not a valid ZIP archive (invalid signatures). |
| `ErrPasswordMismatch` | Incorrect password or missing password for encrypted file. |
| `ErrChecksum` | CRC-32 integrity check failed after reading. |
| `ErrSizeMismatch` | Extracted data size doesn't match the header. |
| `ErrInsecurePath` | **Zip Slip** detected: file path attempts to escape destination. |
| `ErrDuplicateEntry` | A file with this name already exists in the archive. |
| `ErrAlgorithm` | Compression method not supported (e.g., LZMA without plugin). |
| `ErrFileNotFound` | Requested entry is missing. Wraps `fs.ErrNotExist`. |
| `ErrFilenameTooLong` | Filename exceeds the ZIP limit of 65,535 bytes. |

## ⚙️ Configuration & Options

### Functional Options

Configure individual files using the Option pattern:

* `WithName("new_name.txt")`: Rename file inside the archive.
* `WithPath("folder/subfolder")`: Place file inside a specific virtual path.
* `WithCompression(method, level)`: Override compression for this file.
* `WithEncryption(method, password)`: Override encryption for this file.
* `WithMode(0755)`: Set custom file permissions (Unix style).

Configure which files should be saved or extracted using Filters:

* `WithFiles(files)`: Restrict operation to provided files.
* `FromDir("folder1")`: Restrict operation to files located within the specified directory.
* `WithoutDir("folder2")`: Exclude files from a directory from the operation.

Or you can write your own filter, for example to exclude files larger than 10 MB.

### Sort Strategies

* `SortDefault`: Preserves insertion order.
* `SortAlphabetical`: Sorts by name (A-Z).
* `SortSizeDescending`: Optimizes parallel writing.
* `SortZIP64Optimized`: Buckets files by size to optimize Zip64 header overhead.

To optimize memory usage and avoid high heap peaks, use `SortSizeDescending`.
This ensures large buffers are reused efficiently and heavy files don't block the output queue.

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
