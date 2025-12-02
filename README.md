# GoZip

[![Go Reference](https://pkg.go.dev/badge/github.com/lemon4ksan/gozip.svg)](https://pkg.go.dev/github.com/lemon4ksan/gozip)
[![Go Report Card](https://goreportcard.com/badge/github.com/lemon4ksan/gozip)](https://goreportcard.com/report/github.com/lemon4ksan/gozip)

**GoZip** is a high-performance, feature-rich library for creating, reading, and modifying ZIP archives in Go. It is written in pure Go without CGO or external dependencies.

Designed for high-load applications, GoZip focuses on concurrency, memory safety, and strict standard compliance, fixing common pain points found in the standard library (like legacy encodings and WinZip AES compatibility).

## 🚀 Key Features

* **High Performance:** Built-in support for **parallel compression and extraction** using worker pools.
* **Smart I/O:** automatically detects `io.Seeker` interfaces to optimize writing strategies (switching between stream processing and temporary file buffering).
* **Legacy Compatibility:** Includes support for **CP866 (Cyrillic DOS)** and **CP437** encodings for filenames and comments.
* **Security:**
  * **Zip Slip** protection during extraction.
  * **AES-256** (WinZip compatible) and legacy **ZipCrypto** encryption support.
* **Cross-Platform Metadata:** Preserves **NTFS** (Windows) creation/access times and **Unix/macOS** file permissions and timestamps.
* **Standard Compliance:** Full **Zip64** support for files larger than 4GB.
* **Memory Efficient:** Extensive use of `sync.Pool` to minimize GC pressure.
* **Extensible:** Interface-based architecture allowing registration of custom compressors (e.g., Zstd, Brotli).

## 📦 Installation

```bash
go get github.com/lemon4ksan/gozip
```

## 📖 Usage Examples

### 1. Creating an Archive (Sequential)

The simplest way to create an archive. `AddFromPath` is lazy and efficient.

```go
package main

import (
    "os"
    "github.com/lemon4ksan/gozip"
)

func main() {
    // Create a new archive object
    archive := gozip.NewZip()

    // Add a single file
    archive.AddFromPath("document.txt")

    // Add a directory recursively
    // You can override compression level per file/dir
    archive.AddFromDir("images", gozip.WithCompression(gozip.Deflated, gozip.DeflateFast))

    // Create the output file
    out, err := os.Create("backup.zip")
    if err != nil {
        panic(err)
    }
    defer out.Close()

    // Write to disk
    if err := archive.Write(out); err != nil {
        panic(err)
    }
}
```

### 2. Parallel Archiving (High Speed) ⚡

Use `WriteParallel` to utilize multiple CPU cores. This significantly speeds up compression for large datasets.

```go
package main

import (
    "os"
    "runtime"
    "github.com/lemon4ksan/gozip"
)

func main() {
    archive := gozip.NewZip()
    archive.AddFromDir("huge_dataset")

    out, _ := os.Create("data.zip")
    defer out.Close()

    // Use all available CPU cores
    // Max workers control memory usage vs speed
    err := archive.WriteParallel(out, runtime.NumCPU())
    if err != nil {
        panic(err)
    }
}
```

### 3. Encryption (AES-256) 🔒

GoZip supports strong encryption compatible with WinZip and 7-Zip.

```go
func main() {
    archive := gozip.NewZip()

    // Set global configuration
    archive.SetConfig(gozip.ZipConfig{
        CompressionMethod: gozip.Deflated,
        CompressionLevel:  gozip.DeflateNormal,
        EncryptionMethod:  gozip.AES256, // Recommended over ZipCrypto
        Password:          "MySecretPassword123",
    })

    archive.AddFromPath("secret_contract.pdf")
    
    out, _ := os.Create("secure.zip")
    archive.Write(out)
}
```

### 4. Extracting an Archive (Parallel)

Automatically handles directory creation, permissions, and Zip64 parsing.

```go
func main() {
    archive := gozip.NewZip()
    
    // Open the source zip file
    f, err := os.Open("backup.zip")
    if err != nil {
        panic(err)
    }
    defer f.Close()

    // Read structure
    if err := archive.Read(f); err != nil {
        panic(err)
    }

    // Set password if needed
    archive.SetConfig(gozip.ZipConfig{
        Password: "MySecretPassword123",
    })

    // Extract all files to "out" folder using 8 workers
    err = archive.ExtractParallel("output_folder", 8)
    if err != nil {
        panic(err)
    }
}
```

### 5. Fixing Broken Encodings (CP866 / Russian DOS)

If you have old archives created on Windows/DOS that show up as gibberish (e.g., `ΓÑßΓ.txt`), use the `TextEncoding` option.

```go
func main() {
    archive := gozip.NewZip()

    // Configure fallback encoding for filenames not marked as UTF-8
    archive.SetConfig(gozip.ZipConfig{
        TextEncoding: gozip.DecodeIBM866, // Fixes Cyrillic CP866
    })

    f, _ := os.Open("old_archive.zip")
    defer f.Close()

    // Names will be automatically converted to UTF-8
    archive.Read(f) 
    archive.Extract("output")
}
```

### 6. Adding Files from Memory

```go
import "bytes"

func main() {
    archive := gozip.NewZip()
    
    data := []byte("Hello, World!")
    reader := bytes.NewReader(data)

    // Add file from memory
    archive.AddReader(reader, "virtual/hello.txt", gozip.WithMode(0644))
    
    // ... write archive
}
```

## ⚙️ Configuration & Options

### Functional Options

Configure individual files using the Option pattern:

* `WithName("new_name.txt")`: Rename file inside the archive.
* `WithPath("folder/subfolder")`: Place file inside a specific virtual path.
* `WithCompression(method, level)`: Override compression for this file.
* `WithEncryption(method, password)`: Override encryption for this file.
* `WithMode(0755)`: Set custom file permissions (Unix style).

### Sort Strategies

Optimize archive structure for different scenarios:

* `SortDefault`: Preserves insertion order.
* `SortAlphabetical`: Sorts by name (A-Z).
* `SortLargeFilesFirst`: Optimizes **parallel writing** (starts big tasks first).
* `SortLargeFilesLast`: Optimizes sequential access.
* `SortZIP64Optimized`: Buckets files by size to optimize Zip64 header overhead.

```go
archive.SetConfig(gozip.ZipConfig{
    FileSortStrategy: gozip.SortLargeFilesFirst,
})
```

## 🛠 Custom Compressors

GoZip allows registering custom compression algorithms without modifying the library source.

```go
// Implement gozip.Compressor interface
type ZstdCompressor struct {}
func (z *ZstdCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
    // ... implementation ...
}

func main() {
    archive := gozip.NewZip()
    // Register custom method
    archive.RegisterCompressor(gozip.ZStandard, 0, new(ZstdCompressor))
}
```

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
