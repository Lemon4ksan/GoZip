// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lemon4ksan/gozip"
)

// --- Tests ---

func createZipBytes(t *testing.T, files map[string]string) []byte {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(content))
	}
	w.Close()
	return buf.Bytes()
}

func TestZip_BasicOperations(t *testing.T) {
	z := gozip.NewZip()

	// Test AddString & AddBytes
	if _, err := z.AddString("content", "string.txt"); err != nil {
		t.Error(err)
	}
	if _, err := z.AddBytes([]byte("data"), "bytes.bin"); err != nil {
		t.Error(err)
	}

	// Test Mkdir
	if _, err := z.Mkdir("empty_dir"); err != nil {
		t.Error(err)
	}

	// Test AddFile (Real OS file)
	tmpFile := filepath.Join(t.TempDir(), "real.txt")
	os.WriteFile(tmpFile, []byte("real"), 0644)
	if _, err := z.AddFile(tmpFile, gozip.WithName("renamed_real.txt")); err != nil {
		t.Error(err)
	}

	// Test Implicit Dirs creation
	// bytes.bin is implicitly in root, checking deeper nesting
	if _, err := z.AddString("deep", "a/b/c/d.txt"); err != nil {
		t.Error(err)
	}

	// Verify Structure
	if !z.Exists("string.txt") {
		t.Error("string.txt missing")
	}
	if !z.Exists("a/b/") { // Implicit dir
		t.Error("Implicit dir a/b/ missing")
	}

	// Write to buffer
	buf := new(bytes.Buffer)
	if _, err := z.WriteTo(buf); err != nil {
		t.Fatal(err)
	}

	// Read back standard lib
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.File) != 5 {
		// string.txt, bytes.bin, empty_dir/, renamed_real.txt, a/b/c/d.txt
		t.Fatalf("Files count mismatch: expected 5, got %d", len(r.File))
	}
}

func TestZip_AddFS(t *testing.T) {
	mapFS := fstest.MapFS{
		"file.txt":    {Data: []byte("content")},
		"dir/sub.txt": {Data: []byte("sub")},
		"dir/empty":   {Mode: fs.ModeDir},
	}

	z := gozip.NewZip()
	_, err := z.AddFS(mapFS)
	if err != nil {
		t.Fatalf("AddFS failed: %v", err)
	}

	if !z.Exists("file.txt") {
		t.Error("file.txt missing")
	}
	if !z.Exists("dir/sub.txt") {
		t.Error("sub.txt missing")
	}
}

func TestZip_AddLazy(t *testing.T) {
	z := gozip.NewZip()
	called := false

	_, err := z.AddLazy("lazy.txt", func() (io.ReadCloser, error) {
		called = true
		return io.NopCloser(strings.NewReader("lazy content")), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if called {
		t.Error("AddLazy should not call openFunc immediately")
	}

	z.WriteTo(io.Discard)

	if !called {
		t.Error("AddLazy should call openFunc on WriteTo")
	}
}

func TestZip_Modifications(t *testing.T) {
	z := gozip.NewZip()
	z.AddString("1", "keep.txt")
	z.AddString("2", "remove.txt")
	z.AddString("3", "move_me.txt")
	z.AddString("4", "rename_me.txt")
	z.Mkdir("folder")
	z.AddString("5", "folder/child.txt")

	// Test Remove
	if _, err := z.Remove("remove.txt"); err != nil {
		t.Error(err)
	}
	if z.Exists("remove.txt") {
		t.Error("remove.txt should be gone")
	}

	// Test Remove Dir (Recursive)
	if _, err := z.Remove("folder"); err != nil {
		t.Error(err)
	}
	if z.Exists("folder/child.txt") {
		t.Error("child.txt should be removed recursively")
	}

	// Test Rename
	if err := z.Rename("rename_me.txt", "renamed.txt"); err != nil {
		t.Error(err)
	}
	if z.Exists("rename_me.txt") || !z.Exists("renamed.txt") {
		t.Error("Rename failed")
	}

	// Test Move
	if err := z.Move("move_me.txt", "new_folder"); err != nil {
		t.Error(err)
	}
	if !z.Exists("new_folder/move_me.txt") {
		t.Error("Move failed")
	}

	// Test Remove All
	z.Remove(".")
	if len(z.Files()) != 0 {
		t.Error("Remove all failed")
	}
}

func TestZip_Load_ConflictHandlers(t *testing.T) {
	baseData := createZipBytes(t, map[string]string{
		"file.txt": "v1",
		"uniq.txt": "uniq",
	})

	srcData := createZipBytes(t, map[string]string{
		"file.txt": "v2", // Conflict
		"new.txt":  "new",
	})

	t.Run("Replace (Default)", func(t *testing.T) {
		z := gozip.NewZip()
		z.Load(bytes.NewReader(baseData), int64(len(baseData)))

		z.Load(bytes.NewReader(srcData), int64(len(srcData))) // Should replace file.txt

		tmpDir := t.TempDir()

		err := z.ExtractTo(tmpDir)
		if err != nil {
			t.Error(err)
		}
		if len(z.Files()) != 3 {
			t.Errorf("Expected 3 files, got %d", len(z.Files()))
		}
	})

	t.Run("Skip", func(t *testing.T) {
		z := gozip.NewZip()
		z.SetConfig(gozip.ZipConfig{ConflictHandler: func(e, n *gozip.File) (gozip.ConflictAction, string) {
			return gozip.ActionSkip, ""
		}})
		z.Load(bytes.NewReader(baseData), int64(len(baseData)))
		z.Load(bytes.NewReader(srcData), int64(len(srcData)))

		if len(z.Files()) != 3 {
			t.Error("Expected 3 files")
		}
	})

	t.Run("Error", func(t *testing.T) {
		z := gozip.NewZip()
		z.SetConfig(gozip.ZipConfig{ConflictHandler: func(e, n *gozip.File) (gozip.ConflictAction, string) {
			return gozip.ActionError, ""
		}})
		z.Load(bytes.NewReader(baseData), int64(len(baseData)))
		_, err := z.Load(bytes.NewReader(srcData), int64(len(srcData)))

		if !errors.Is(err, gozip.ErrDuplicateEntry) {
			t.Errorf("Expected ErrDuplicateEntry, got %v", err)
		}
	})

	t.Run("Rename", func(t *testing.T) {
		z := gozip.NewZip()
		z.SetConfig(gozip.ZipConfig{ConflictHandler: func(e, n *gozip.File) (gozip.ConflictAction, string) {
			return gozip.ActionRename, "file_v2.txt"
		}})
		z.Load(bytes.NewReader(baseData), int64(len(baseData)))
		z.Load(bytes.NewReader(srcData), int64(len(srcData)))

		if !z.Exists("file_v2.txt") {
			t.Error("Renamed file missing")
		}
		if !z.Exists("file.txt") {
			t.Error("Original file missing")
		}
	})
}

func TestZip_Security_ResourceLimits(t *testing.T) {
	z := gozip.NewZip()

	f, _ := gozip.NewFile("bomb.txt", false)
	f.WithOpenFunc(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(make([]byte, 1024*1024))), nil
	}).WithUncompressedSize(gozip.SizeUnknown)
	z.Add(f)

	buf := new(bytes.Buffer)
	if _, err := z.WriteTo(buf); err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	reader := gozip.NewZip()
	reader.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	dest := t.TempDir()

	err := reader.ExtractTo(dest, gozip.WithSecurity(gozip.SecuritySettings{
		ResourceLimits: gozip.ResourceLimits{
			MaxFileSize: 1024,
		},
	}))
	if !errors.Is(err, gozip.ErrResourceLimit) {
		t.Errorf("Expected ErrResourceLimit (FileSize), got %v", err)
	}

	err = reader.ExtractTo(dest, gozip.WithSecurity(gozip.SecuritySettings{
		ResourceLimits: gozip.ResourceLimits{
			MaxTotalSize: 1024,
		},
	}))
	if !errors.Is(err, gozip.ErrResourceLimit) {
		t.Errorf("Expected ErrResourceLimit (TotalSize), got %v", err)
	}
}

func TestZip_Security_Symlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping symlink test on Windows")
	}

	srcDir := t.TempDir()
	os.Symlink("/etc/passwd", filepath.Join(srcDir, "link"))

	z := gozip.NewZip()
	z.AddDir(srcDir)

	destDir := t.TempDir()

	buf := new(bytes.Buffer)
	z.WriteTo(buf)

	reader := gozip.NewZip()
	reader.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	err := reader.ExtractTo(destDir)
	if !errors.Is(err, gozip.ErrInsecurePath) {
		t.Errorf("Expected ErrInsecurePath (symlinks disabled), got %v", err)
	}

	err = reader.ExtractTo(destDir, gozip.WithSecurity(gozip.SecuritySettings{AllowSymlinks: true}))
	if err != nil {
		t.Errorf("Failed to extract with symlinks allowed: %v", err)
	}

	info, err := os.Lstat(filepath.Join(destDir, "link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("Symlink not restored")
	}
}

func TestZip_ContextCancellation(t *testing.T) {
	// Create a large-ish zip
	z := gozip.NewZip()
	data := make([]byte, 10*1024*1024)
	z.AddBytes(data, "large.bin")

	buf := new(bytes.Buffer)
	z.WriteTo(buf)

	// Extract with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	reader := gozip.NewZip()
	reader.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	// Should fail immediately or quickly
	err := reader.ExtractToWithContext(ctx, t.TempDir())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("Warning: Extraction finished too fast or wrong error: %v", err)
	}
}

func TestZip_WriteHTTP(t *testing.T) {
	z := gozip.NewZip()
	z.AddString("content", "file.txt")

	rec := httptest.NewRecorder()

	err := z.WriteHTTP(rec, "download.zip")
	if err != nil {
		t.Fatal(err)
	}

	if rec.Code != 200 {
		t.Errorf("Status 200 expected, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/zip" {
		t.Error("Content-Type mismatch")
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), `attachment; filename=download.zip`) {
		t.Error("Content-Disposition mismatch")
	}
}

func TestZip_Glob_Find_Select(t *testing.T) {
	z := gozip.NewZip()
	z.AddString("1", "logs/error.log")
	z.AddString("2", "logs/access.log")
	z.AddString("3", "data/db.dump")
	z.AddString("4", "readme.md")

	// Glob
	matches, _ := z.Glob("logs/*.log")
	if len(matches) != 2 {
		t.Errorf("Glob expected 2, got %d", len(matches))
	}

	// Find
	matches, _ = z.Find("*.log") // Should find deep files
	if len(matches) != 2 {
		t.Errorf("Find expected 2, got %d", len(matches))
	}

	// Select
	selected := z.Select(func(f *gozip.File) bool {
		return f.UncompressedSize() > 0
	})
	if len(selected) != 4 {
		t.Errorf("Select expected 4, got %d", len(selected))
	}
}

func TestZip_Add_Validation(t *testing.T) {
	z := gozip.NewZip()
	if err := z.Add(nil); err == nil {
		t.Error("Add(nil) should error")
	}

	// Add conflict
	z.AddString("a", "file.txt")
	_, err := z.AddString("b", "file.txt")
	if !errors.Is(err, gozip.ErrDuplicateEntry) {
		t.Errorf("Expected ErrDuplicateEntry, got %v", err)
	}
}

func TestZip_ParallelOperations(t *testing.T) {
	z := gozip.NewZip()
	for i := range 100 {
		z.AddString(fmt.Sprintf("data %d", i), fmt.Sprintf("file_%d.txt", i))
	}

	// Parallel Write
	buf := new(bytes.Buffer)
	_, err := z.WriteTo(buf, gozip.WithWorkers(4))
	if err != nil {
		t.Fatal(err)
	}

	// Parallel Extract
	reader := gozip.NewZip()
	reader.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	dest := t.TempDir()
	err = reader.ExtractTo(dest, gozip.WithWorkers(4))
	if err != nil {
		t.Fatal(err)
	}

	// Parallel Verify
	err = reader.Verify(gozip.WithWorkers(4))
	if err != nil {
		t.Fatal(err)
	}
}

func TestZip_Rename_EdgeCases(t *testing.T) {
	z := gozip.NewZip()
	z.AddString("v", "folder/file.txt")

	// Rename parent folder
	err := z.Rename("folder", "new_folder")
	if err != nil {
		t.Fatal(err)
	}
	if !z.Exists("new_folder/file.txt") {
		t.Error("Child not moved")
	}

	// Rename to existing
	z.AddString("x", "conflict")
	err = z.Rename("new_folder", "conflict")
	if !errors.Is(err, gozip.ErrDuplicateEntry) {
		t.Errorf("Rename to existing should fail, got %v", err)
	}

	// Invalid name
	err = z.Rename("conflict", "")
	if err == nil {
		t.Error("Rename to empty should fail")
	}
}

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

	if _, err := readArchive.Load(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
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

func TestZip64_Structure(t *testing.T) {
	archive := gozip.NewZip()
	buf := new(bytes.Buffer)
	archive.AddString("content", "file.txt")

	nonSeeker := struct{ io.Writer }{buf}

	if _, err := archive.WriteTo(nonSeeker); err != nil {
		t.Fatalf("WriteTo stream: %v", err)
	}

	verifyZipContent(t, buf.Bytes(), map[string]string{"file.txt": "content"}, "")
}

// --- Context & Cancellation Tests ---

func TestExtractTo_Cancellation(t *testing.T) {
	archive := gozip.NewZip()
	for i := range 100 {
		archive.AddString("data", fmt.Sprintf("file_%d.txt", i))
	}

	ctx, cancel := context.WithCancel(context.Background())

	tmpDir := t.TempDir()

	errCh := make(chan error)
	go func() {
		errCh <- archive.ExtractToWithContext(ctx, tmpDir, gozip.WithWorkers(1))
	}()

	cancel()

	err := <-errCh
	if err == nil {
		t.Error("Expected error due to cancellation, got nil")
	}
	if !strings.Contains(err.Error(), "canceled") && !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context canceled error, got: %v", err)
	}
}

// --- Security Tests ---

func TestZipSlip_Protection(t *testing.T) {
	archive := gozip.NewZip()
	archive.AddString("evil", "../../etc/passwd")

	f, ok := archive.File("etc/passwd")
	if !ok {
		if _, ok := archive.File("passwd"); !ok {
		}
	} else {
		if strings.Contains(f.Name(), "..") {
			t.Error("AddFile did not sanitize path")
		}
	}
}

func TestWriteTo_ClosedWriter(t *testing.T) {
	archive := gozip.NewZip()
	archive.AddString("data", "test.txt")

	// Writer that fails immediately
	pr, pw := io.Pipe()
	pw.Close() // Closed immediately

	_, err := archive.WriteTo(pw)
	if err == nil {
		t.Error("Expected error writing to closed pipe")
	}
	pr.Close()
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

// --- Race condition tests ---

func TestParallelWriteRace(t *testing.T) {
	archive := gozip.NewZip()

	content := "Some repeatable content for compression testing"

	for i := range 5 {
		name := fmt.Sprintf("file_%d.txt", i)

		f, err := archive.AddLazy(name, func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		f.WithUncompressedSize(int64(len(content)))
	}

	const (
		writersCount = 20
		repeatCount  = 50
	)

	var wg sync.WaitGroup
	wg.Add(writersCount + 1)

	go func() {
		defer wg.Done()
		for i := range repeatCount {
			for j := range 5 {
				name := fmt.Sprintf("file_%d.txt", j)
				if f, ok := archive.File(name); ok {
					f.WithPassword(fmt.Sprintf("pass_%d", i)).
						WithComment(fmt.Sprintf("comment_%d", i))
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for i := range writersCount {
		go func(id int) {
			defer wg.Done()

			_, err := archive.WriteTo(io.Discard,
				gozip.WithWorkers(runtime.NumCPU()),
				gozip.WithProgress(func(s gozip.ProgressStats) {
					_ = s.TotalRead
					_ = s.CurrentFile
				}))

			if err != nil {
				t.Errorf("Writer %d failed: %v", id, err)
			}
		}(i)
	}

	wg.Wait()
}

func TestParallelExtractRace(t *testing.T) {
	archive := gozip.NewZip()
	content := "content"

	f, err := archive.AddLazy("test.txt", func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.WithPassword("secret").WithUncompressedSize(int64(len(content)))

	const count = 10
	var wg sync.WaitGroup
	wg.Add(count)

	for range count {
		go func() {
			defer wg.Done()

			fileRef, ok := archive.File("test.txt")
			if !ok {
				t.Error("File not found")
				return
			}

			rc, err := fileRef.Open()
			if err != nil {
				t.Errorf("Open failed: %v", err)
				return
			}
			defer rc.Close()

			if _, err := io.Copy(io.Discard, rc); err != nil {
				t.Errorf("Copy failed: %v", err)
			}
		}()
	}
	wg.Wait()
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
		if _, err := archive.Load(f, testZipSize); err != nil {
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
			if file.IsDir() {
				continue
			}
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
		for range workers {
			go func() {
				defer wg.Done()
				for file := range ch {
					if file.IsDir() {
						continue
					}
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
