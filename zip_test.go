package gozip_test

import (
	"archive/zip"
	"compress/flate"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"testing"

	"github.com/lemon4ksan/gozip"
)

const (
	smallFileCount = 1000
	smallFileSize  = 1 * 1024 // 1KB

	mediumFileCount = 10
	mediumFileSize  = 10 * 1024 * 1024 // 10MB
)

var (
	smallFiles  []testFile
	mediumFiles []testFile
)

type testFile struct {
	name string
	body []byte
}

func init() {
	fmt.Println("Generating test data...")
	smallFiles = generateFiles(smallFileCount, smallFileSize)
	mediumFiles = generateFiles(mediumFileCount, mediumFileSize)
	fmt.Println("Data generated.")
}

func generateFiles(count, size int) []testFile {
	files := make([]testFile, count)
	rng := rand.New(rand.NewSource(42))

	baseContent := make([]byte, size)
	for i := range baseContent {
		baseContent[i] = byte(rng.Intn(26) + 'a') // a-z
	}

	for i := 0; i < count; i++ {
		files[i] = testFile{
			name: fmt.Sprintf("file_%d.txt", i),
			body: baseContent, // Copying slice header only, content is shared (read-only)
		}
	}
	return files
}

// --- Benchmark: Many Small Files ---

func BenchmarkWrite_Small_StdLib(b *testing.B) {
	runStdLibBenchmark(b, smallFiles)
}

func BenchmarkWrite_Small_GoZip_Seq(b *testing.B) {
	runGoZipSeqBenchmark(b, smallFiles)
}

func BenchmarkWrite_Small_GoZip_Par(b *testing.B) {
	runGoZipParBenchmark(b, smallFiles)
}

// --- Benchmark: Medium Files (CPU Bound) ---

func BenchmarkWrite_Medium_StdLib(b *testing.B) {
	runStdLibBenchmark(b, mediumFiles)
}

func BenchmarkWrite_Medium_GoZip_Seq(b *testing.B) {
	runGoZipSeqBenchmark(b, mediumFiles)
}

func BenchmarkWrite_Medium_GoZip_Par(b *testing.B) {
	runGoZipParBenchmark(b, mediumFiles)
}

// --- Helpers ---

func runStdLibBenchmark(b *testing.B, files []testFile) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		zw := zip.NewWriter(io.Discard)

		for _, f := range files {
			w, err := zw.CreateHeader(&zip.FileHeader{
				Name:   f.name,
				Method: zip.Deflate,
			})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := w.Write(f.body); err != nil {
				b.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func runGoZipSeqBenchmark(b *testing.B, files []testFile) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		archive := gozip.NewZip()
		archive.SetConfig(gozip.ZipConfig{
			CompressionMethod: gozip.Deflated,
			CompressionLevel:  flate.DefaultCompression,
		})

		for _, f := range files {
			err := archive.AddBytes(f.body, f.name)
			if err != nil {
				b.Fatal(err)
			}
		}

		if err := archive.Write(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func runGoZipParBenchmark(b *testing.B, files []testFile) {
	b.ReportAllocs()
	workers := runtime.NumCPU()

	for i := 0; i < b.N; i++ {
		archive := gozip.NewZip()
		archive.SetConfig(gozip.ZipConfig{
			CompressionMethod: gozip.Deflated,
			CompressionLevel:  flate.DefaultCompression,
		})

		for _, f := range files {
			archive.AddBytes(f.body, f.name)
		}

		if err := archive.WriteParallel(io.Discard, workers); err != nil {
			b.Fatal(err)
		}
	}
}
