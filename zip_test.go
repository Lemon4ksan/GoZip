// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/lemon4ksan/gozip"
)

// --- Integration Tests ---

func TestRoundTrip_Sequential(t *testing.T) {
	buf := new(bytes.Buffer)
	archive := gozip.NewZip()
	archive.SetConfig(gozip.ZipConfig{
		CompressionMethod: gozip.Deflate,
		CompressionLevel:  gozip.DeflateNormal,
		Comment:           "Test Archive",
		UseImplicitDirs:   true,
	})

	testFiles := map[string]string{
		"hello.txt":       "Hello World",
		"dir/nested.json": "{}",
		"images/logo.png": string([]byte{0x89, 0x50, 0x4E, 0x47}), // Fake PNG signature
	}

	for name, content := range testFiles {
		if strings.HasSuffix(name, "/") {
			if _, err := archive.Mkdir(name); err != nil {
				t.Fatalf("Mkdir(%s): %v", name, err)
			}
			continue
		}
		if _, err := archive.AddString(content, name); err != nil {
			t.Fatalf("AddString(%s): %v", name, err)
		}
	}

	if _, err := archive.WriteTo(buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	verifyZipContent(t, buf.Bytes(), testFiles, "Test Archive")
}

func TestRoundTrip_Parallel(t *testing.T) {
	buf := new(bytes.Buffer)
	archive := gozip.NewZip()

	count := 50
	files := make(map[string]string)
	for i := range count {
		name := fmt.Sprintf("file_%d.txt", i)
		content := strings.Repeat("data ", i+1)
		files[name] = content
		archive.AddString(content, name)
	}

	// 4 workers ensure we test concurrency logic
	if _, err := archive.WriteTo(buf, gozip.WithWorkers(4)); err != nil {
		t.Fatalf("WriteToParallel: %v", err)
	}

	verifyZipContent(t, buf.Bytes(), files, "")
}

func TestRoundTrip_AES256(t *testing.T) {
	password := "secure_pass"
	buf := new(bytes.Buffer)

	archive := gozip.NewZip()
	archive.SetConfig(gozip.ZipConfig{
		EncryptionMethod: gozip.AES256,
		Password:         password,
	})

	name := "secret.txt"
	content := "This is a secret message"
	archive.AddString(content, name)

	if _, err := archive.WriteTo(buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	// Read back using gozip to verify decryption logic
	readArchive := gozip.NewZip()
	readArchive.SetConfig(gozip.ZipConfig{Password: password})

	if err := readArchive.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("Load: %v", err)
	}

	f, ok := readArchive.File(name)
	if !ok {
		t.Fatalf("File not found")
	}

	rc, err := f.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != content {
		t.Errorf("Decrypted content mismatch: got %q, want %q", string(got), content)
	}
}

// verifyZipContent uses standard library to ensure compatibility
func verifyZipContent(t *testing.T, data []byte, expectedFiles map[string]string, expectedComment string) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("std lib zip.NewReader: %v", err)
	}

	if r.Comment != expectedComment {
		t.Errorf("Comment mismatch: got %q, want %q", r.Comment, expectedComment)
	}

	if len(r.File) != len(expectedFiles) {
		t.Errorf("File count mismatch: got %d, want %d", len(r.File), len(expectedFiles))
	}

	for _, f := range r.File {
		expectedContent, ok := expectedFiles[f.Name]
		if !ok {
			// Stdlib sometimes strips trailing slashes for dirs
			if f.FileInfo().IsDir() && expectedFiles[f.Name+"/"] != "" {
				continue
			}
			t.Errorf("Unexpected file in archive: %s", f.Name)
			continue
		}

		if f.FileInfo().IsDir() {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			t.Fatalf("std lib f.Open(%s): %v", f.Name, err)
		}

		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("std lib ReadAll(%s): %v", f.Name, err)
		}

		if string(got) != expectedContent {
			t.Errorf("Content mismatch for %s", f.Name)
		}
	}
}

// --- Benchmarks ---

const (
	smallFileCount  = 1000
	smallFileSize   = 1 * 1024 // 1KB
	mediumFileCount = 10
	mediumFileSize  = 10 * 1024 * 1024 // 10MB
)

type testFile struct {
	name string
	body []byte
}

var (
	smallFiles  []testFile
	mediumFiles []testFile
	testZipPath string
	testZipSize int64
)

func TestMain(m *testing.M) {
	// Generate compressible data.
	// Random data (rand.Read) is bad for benchmarks because Deflate will skip it or work differently.
	// We want to stress the CPU compression algorithm.
	smallFiles = generateFiles(smallFileCount, smallFileSize)
	mediumFiles = generateFiles(mediumFileCount, mediumFileSize)

	// Setup a real ZIP file on disk for Load/Read benchmarks
	f, err := os.CreateTemp("", "bench_read_*.zip")
	if err != nil {
		panic(err)
	}
	testZipPath = f.Name()

	archive := gozip.NewZip()
	// ~1KB of compressible text
	content := strings.Repeat("GoZip library benchmark testing. ", 30)

	for i := range 500 {
		name := fmt.Sprintf("folder_%d/file_%d.txt", i%10, i)
		archive.AddString(content, name)
	}

	if _, err := archive.WriteTo(f); err != nil {
		panic(err)
	}
	f.Close()

	info, _ := os.Stat(testZipPath)
	testZipSize = info.Size()

	code := m.Run()

	os.Remove(testZipPath)
	os.Exit(code)
}

func generateFiles(count, size int) []testFile {
	files := make([]testFile, count)

	// Create a compressible pattern (e.g. repeated text/code)
	pattern := []byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ")
	body := bytes.Repeat(pattern, (size/len(pattern))+1)[:size]

	for i := range count {
		files[i] = testFile{
			name: fmt.Sprintf("file_%d.txt", i),
			body: body, // Shared underlying array to save test memory, fine for reading
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

// --- Benchmark Helpers ---

func runStdLibBenchmark(b *testing.B, files []testFile) {
	b.ReportAllocs()
	for b.Loop() {
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
	for b.Loop() {
		archive := gozip.NewZip()
		archive.SetConfig(gozip.ZipConfig{
			CompressionMethod: gozip.Deflate,
			CompressionLevel:  gozip.DeflateNormal,
		})

		// AddBytes does CRC calculation, so it's part of the workload
		for _, f := range files {
			if _, err := archive.AddBytes(f.body, f.name); err != nil {
				b.Fatal(err)
			}
		}

		if _, err := archive.WriteTo(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func runGoZipParBenchmark(b *testing.B, files []testFile) {
	b.ReportAllocs()
	workers := runtime.NumCPU()
	ctx := context.Background()

	for b.Loop() {
		archive := gozip.NewZip()
		archive.SetConfig(gozip.ZipConfig{
			CompressionMethod: gozip.Deflate,
			CompressionLevel:  gozip.DeflateNormal,
		})

		for _, f := range files {
			if _, err := archive.AddBytes(f.body, f.name); err != nil {
				b.Fatal(err)
			}
		}

		if _, err := archive.WriteToWithContext(ctx, io.Discard, gozip.WithWorkers(workers)); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Benchmark: Load/Metadata ---

func BenchmarkLoad_StdLib(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		r, err := zip.OpenReader(testZipPath)
		if err != nil {
			b.Fatal(err)
		}
		// Access something to ensure it's loaded
		_ = len(r.File)
		r.Close()
	}
}

func BenchmarkLoad_GoZip(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		archive := gozip.NewZip()
		f, err := os.Open(testZipPath)
		if err != nil {
			b.Fatal(err)
		}
		// GoZip Load reads and parses Central Directory completely
		if err := archive.Load(f, testZipSize); err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

// --- Benchmark: Read/Decompress ---

func BenchmarkReadSeq_GoZip(b *testing.B) {
	f, _ := os.Open(testZipPath)
	defer f.Close()

	archive := gozip.NewZip()
	archive.Load(f, testZipSize)
	files := archive.Files()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		for _, file := range files {
			rc, _ := file.Open()
			io.Copy(io.Discard, rc)
			rc.Close()
		}
	}
}

func BenchmarkReadPar_GoZip(b *testing.B) {
	f, _ := os.Open(testZipPath)
	defer f.Close()

	archive := gozip.NewZip()
	archive.Load(f, testZipSize)
	files := archive.Files()
	workers := runtime.NumCPU()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var wg sync.WaitGroup
		ch := make(chan *gozip.File, len(files))
		for _, file := range files {
			ch <- file
		}
		close(ch)

		wg.Add(workers)
		for j := 0; j < workers; j++ {
			go func() {
				defer wg.Done()
				for file := range ch {
					rc, _ := file.Open()
					io.Copy(io.Discard, rc)
					rc.Close()
				}
			}()
		}
		wg.Wait()
	}
}

// --- Benchmark StreamReader (Streaming) ---

func BenchmarkStreamReader_GoZip(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		f, err := os.Open(testZipPath)
		if err != nil {
			b.Fatal(err)
		}

		sr := gozip.NewStreamReader(f)
		for {
			_, err := sr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}

			// Full decompression cycle
			rc, err := sr.Open()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				b.Fatal(err)
			}
			rc.Close()
		}
		f.Close()
	}
}

// --- Benchmark: Extraction (Parallel) ---

func BenchmarkExtractParallel_GoZip(b *testing.B) {
	tempDir, _ := os.MkdirTemp("", "gozip_extract_*")
	defer os.RemoveAll(tempDir)

	f, _ := os.Open(testZipPath)
	defer f.Close()
	archive := gozip.NewZip()
	archive.Load(f, testZipSize)

	workers := runtime.NumCPU()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		// Note: We are overwriting files in the same temp dir.
		// This is fine for benchmarking throughput.
		if err := archive.ExtractTo(tempDir, gozip.WithWorkers(workers)); err != nil {
			b.Fatal(err)
		}
	}
}
