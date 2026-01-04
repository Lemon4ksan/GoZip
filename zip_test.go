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
	"math/rand"
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
	})

	testFiles := map[string]string{
		"hello.txt":       "Hello World",
		"dir/":            "",
		"dir/nested.json": "{}",
		"images/":         "",
		"images/logo.png": string([]byte{0x89, 0x50, 0x4E, 0x47}),
	}

	for name, content := range testFiles {
		if strings.HasSuffix(name, "/") {
			continue
		}
		if err := archive.AddString(content, name); err != nil {
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
		content := strings.Repeat("data", i+1)
		files[name] = content
		archive.AddString(content, name)
	}

	if _, err := archive.WriteToParallel(buf, 4); err != nil {
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

	readArchive := gozip.NewZip()
	readArchive.SetConfig(gozip.ZipConfig{Password: password})

	if err := readArchive.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("Load: %v", err)
	}

	f, err := readArchive.File(name)
	if err != nil {
		t.Fatalf("File not found: %v", err)
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

// verifyZipContent helper using standard library to ensure compatibility
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
			t.Errorf("Unexpected file in archive: %s", f.Name)
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

const (
	readFileCount = 1000
)

var (
	testZipPath string
	testZipSize int64
)

func init() {
	// Generate data once for consistent benchmarks
	smallFiles = generateFiles(smallFileCount, smallFileSize)
	mediumFiles = generateFiles(mediumFileCount, mediumFileSize)

	envPath := os.Getenv("TEST_ZIP_PATH")
	if envPath != "" {
		info, err := os.Stat(envPath)
		if err == nil {
			testZipPath = envPath
			testZipSize = info.Size()
			fmt.Printf("Benchmark: Using external file: %s (%d MB)\n", envPath, testZipSize/1024/1024)
			return
		}
	}

	fmt.Println("Benchmark: Generating temporary test archive...")
	f, err := os.CreateTemp("", "bench_read_*.zip")
	if err != nil {
		panic(err)
	}
	testZipPath = f.Name()

	w := gozip.NewZip()
	content := strings.Repeat("A regular repeating string for compression testing. ", 20) // ~1KB

	for i := range readFileCount {
		name := fmt.Sprintf("folder_%d/file_%d.txt", i%10, i)
		w.AddString(content, name)
	}

	if _, err := w.WriteTo(f); err != nil {
		panic(err)
	}
	f.Close()

	stat, _ := os.Stat(testZipPath)
	testZipSize = stat.Size()
	fmt.Printf("Benchmark: Generated %s (%d files, %.2f MB)\n",
		testZipPath, readFileCount, float64(testZipSize)/1024/1024)
}

func generateFiles(count, size int) []testFile {
	files := make([]testFile, count)
	rng := rand.New(rand.NewSource(42))

	baseContent := make([]byte, size)
	for i := range baseContent {
		baseContent[i] = byte(rng.Intn(26) + 'a') // a-z
	}

	for i := range count {
		files[i] = testFile{
			name: fmt.Sprintf("file_%d.txt", i),
			body: baseContent, // Shared underlying array to save test memory
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

		for _, f := range files {
			if err := archive.AddBytes(f.body, f.name); err != nil {
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
			if err := archive.AddBytes(f.body, f.name); err != nil {
				b.Fatal(err)
			}
		}

		if _, err := archive.WriteToParallelWithContext(ctx, io.Discard, workers); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad_StdLib(b *testing.B) {
	for b.Loop() {
		r, err := zip.OpenReader(testZipPath)
		if err != nil {
			b.Fatal(err)
		}
		_ = len(r.File)
		r.Close()
	}
}

func BenchmarkLoad_GoZip(b *testing.B) {
	for b.Loop() {
		archive := gozip.NewZip()
		f, err := os.Open(testZipPath)
		if err != nil {
			b.Fatal(err)
		}

		err = archive.Load(f, testZipSize)
		if err != nil {
			b.Fatal(err)
		}
		f.Close()
	}
}

func BenchmarkReadSeq_StdLib(b *testing.B) {
	r, err := zip.OpenReader(testZipPath)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()

	for b.Loop() {
		for _, f := range r.File {
			rc, err := f.Open()
			if err != nil {
				b.Fatal(err)
			}
			if f.UncompressedSize64 == 0 {
				continue
			}
			_, err = io.Copy(io.Discard, rc)
			rc.Close()
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkReadSeq_GoZip(b *testing.B) {
	f, err := os.Open(testZipPath)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	archive := gozip.NewZip()
	if err := archive.Load(f, testZipSize); err != nil {
		b.Fatal(err)
	}
	files := archive.Files()

	for b.Loop() {
		for _, file := range files {
			rc, err := file.Open()
			if err != nil {
				b.Fatal(err)
			}
			if file.UncompressedSize() == 0 {
				continue
			}
			_, err = io.Copy(io.Discard, rc)
			rc.Close()
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkReadPar_GoZip(b *testing.B) {
	f, err := os.Open(testZipPath)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	archive := gozip.NewZip()
	if err := archive.Load(f, testZipSize); err != nil {
		b.Fatal(err)
	}
	files := archive.Files()
	workers := runtime.NumCPU()

	for b.Loop() {
		var wg sync.WaitGroup
		queue := make(chan *gozip.File, len(files))

		for _, file := range files {
			queue <- file
		}
		close(queue)

		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				for file := range queue {
					rc, err := file.Open()
					if err != nil {
						b.Errorf("Open error: %v", err)
						return
					}
					if file.UncompressedSize() == 0 {
						continue
					}
					if _, err := io.Copy(io.Discard, rc); err != nil {
						b.Errorf("Copy error: %v", err)
					}
					rc.Close()
				}
			}()
		}
		wg.Wait()
	}
}
