# GoZip

**GoZip** is a high-performance, feature-rich library for creating, reading, and modifying ZIP archives in Go without any external dependencies. It is designed with concurrency, memory efficiency, and security in mind, making it suitable for high-load applications.

## 🚀 Key Features

* **High Performance:** Support for **parallel compression and extraction** using worker pools.
* **Memory Efficient:** Extensive use of `sync.Pool` and streaming interfaces to minimize GC pressure.
* **Smart I/O:** Automatically detects `io.Seeker` interfaces to optimize writing strategies (avoiding temporary files when possible).
* **Security:** Built-in protection against **Zip Slip** vulnerabilities during extraction.
* **Encryption:** Support for legacy **ZipCrypto** and modern **AES-256** encryption.
* **Standard Compliance:** Full **Zip64** support for large files (>4GB) and preservation of file attributes (permissions, modification times).
* **Flexibility:** Functional options pattern for granular control over individual files.
* **Extensible:** Interface-based architecture allowing registration of custom compressors (e.g., Zstd, Brotli).

## 📦 Installation

```bash
go get github.com/lemon4ksan/gozip
```

## 📖 Usage Examples

### 1. Creating an Archive (Sequential)

The simplest way to create an archive. `AddFromPath` is lazy and doesn't open the file until writing begins.

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

### 2. Parallel Archiving (Fast) ⚡

Use `WriteParallel` to utilize multiple CPU cores. This is significantly faster for large archives.

```go
package main

import (
    "os"
    "runtime"
    "github.com/lemon4ksan/gozip"
)

func main() {
    archive := gozip.NewZip()
    archive.AddFromDir("large_dataset")

    out, _ := os.Create("data.zip")
    defer out.Close()

    // Use all available CPU cores
    err := archive.WriteParallel(out, runtime.NumCPU())
    if err != nil {
        panic(err)
    }
}
```

### 3. Encryption (AES-256) 🔒

GoZip supports encrypting the entire archive or specific files.

```go
func main() {
    archive := gozip.NewZip()

    // Set global configuration
    archive.SetConfig(gozip.ZipConfig{
        CompressionMethod: gozip.Deflated,
        CompressionLevel:  gozip.DeflateNormal,
        EncryptionMethod:  gozip.AES256,
        Password:          "MySecretPassword123",
    })

    archive.AddFromPath("secret.pdf")
    
    out, _ := os.Create("secure.zip")
    archive.Write(out)
}
```

### 4. Extracting an Archive

You can extract files sequentially or in parallel. The library automatically handles directory creation and permission restoration.

```go
func main() {
    archive := gozip.NewZip()
    
    // Open the source zip file
    f, err := os.Open("backup.zip")
    if err != nil {
        panic(err)
    }
    defer f.Close()

    // Read the Central Directory
    if err := archive.Read(f); err != nil {
        panic(err)
    }

    // Set password if the archive is encrypted
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

### 5. Adding Files from Memory

You don't need a physical file to add data to the archive. Use `AddReader`.

```go
import "bytes"

func main() {
    archive := gozip.NewZip()
    
    data := []byte("Hello, World!")
    reader := bytes.NewReader(data)

    // Add file from memory
    archive.AddReader(reader, "hello/world.txt")
    
    // ... write archive
}
```

## ⚙️ Configuration & Options

### Functional Options

You can configure individual files using options:

* `WithName("new_name.txt")`: Rename file inside archive.
* `WithPath("folder/subfolder")`: Place file inside a specific virtual folder.
* `WithCompression(method, level)`: Set specific compression for this file.
* `WithEncryption(method, password)`: Set specific encryption for this file.
* `WithMode(0755)`: Set custom file permissions.

### Sort Strategies

Control how files are ordered in the archive (useful for optimization):

* `SortDefault`: Keeps insertion order.
* `SortAlphabetical`: Sorts by name (A-Z).
* `SortLargeFilesFirst`: Optimizes parallel compression speed.
* `SortZIP64Optimized`: Optimizes internal offsets for Zip64 archives.

```go
archive.SetConfig(gozip.ZipConfig{
    FileSortStrategy: gozip.SortLargeFilesFirst,
})
```

## 🛠 Custom Compressors

GoZip is designed to be extensible. You can register custom implementations (e.g., Zstd or faster Deflate) without forking the library.

```go
// Implement gozip.Compressor interface
type ZstdCompressor struct {}
func (z *ZstdCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
    // ... implementation using a zstd library ...
}

func main() {
    archive := gozip.NewZip()
    
    // Register custom method
    archive.RegisterCompressor(gozip.ZStandard, 0, new(ZstdCompressor))
}
```

## License

This code is licensed under the same conditions as the original Go code. See [LICENSE](LICENSE) file.
