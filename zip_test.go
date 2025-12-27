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
	"runtime"
	"strings"
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
		"images/":          "",
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

func init() {
	// Generate data once for consistent benchmarks
	smallFiles = generateFiles(smallFileCount, smallFileSize)
	mediumFiles = generateFiles(mediumFileCount, mediumFileSize)
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

	for i := 0; i < b.N; i++ {
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
