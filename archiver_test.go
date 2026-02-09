package gozip_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemon4ksan/gozip"
)

func createTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "gozip-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func createTestFiles(t *testing.T, dir string, files map[string]string) {
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiver_RoundTrip_Dir(t *testing.T) {
	// 1. Setup Input
	srcDir := createTempDir(t)
	createTestFiles(t, srcDir, map[string]string{
		"file1.txt":       "hello world",
		"subdir/data.csv": "id,value\n1,100",
	})

	destZip := filepath.Join(createTempDir(t), "archive.zip")
	destDir := createTempDir(t)

	// 2. Archive
	err := gozip.ArchiveDir(srcDir, gozip.ToFilePath(destZip))
	if err != nil {
		t.Fatalf("ArchiveDir failed: %v", err)
	}

	// 3. Unzip
	err = gozip.Unzip(gozip.FromFilePath(destZip), destDir)
	if err != nil {
		t.Fatalf("Unzip failed: %v", err)
	}

	// 4. Verify
	content, _ := os.ReadFile(filepath.Join(destDir, "file1.txt"))
	if string(content) != "hello world" {
		t.Errorf("file1.txt content mismatch")
	}
	content, _ = os.ReadFile(filepath.Join(destDir, "subdir/data.csv"))
	if string(content) != "id,value\n1,100" {
		t.Errorf("subdir/data.csv content mismatch")
	}
}

func TestArchiver_MemoryFlow(t *testing.T) {
	// Test Source/Sink abstractions (Bytes -> Zip -> Bytes)
	var buf bytes.Buffer
	content := []byte("memory content")

	// Create Archive in memory
	archive := gozip.NewZip()
	archive.AddBytes(content, "test.txt")

	sink := gozip.ToWriter(&buf)
	w, _ := sink.Create()
	_, err := archive.WriteTo(w)
	if err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	// Read from memory
	// Important: To use FromReader with bytes.Buffer, we need a ReaderAt.
	// bytes.NewReader provides ReadAt.
	reader := bytes.NewReader(buf.Bytes())
	src := gozip.FromReaderAt(reader, int64(reader.Len()))

	// High-level read
	readBack, err := gozip.ReadFile(src, "test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if !bytes.Equal(readBack, content) {
		t.Errorf("Content mismatch. Want %s, got %s", content, readBack)
	}
}

func TestArchiver_HighLevelHelpers(t *testing.T) {
	// Setup
	tmpDir := createTempDir(t)
	zipPath := filepath.Join(tmpDir, "data.zip")

	type Config struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	cfg := Config{Host: "localhost", Port: 8080}
	cfgData, _ := json.Marshal(cfg)

	// Create Zip manually
	z := gozip.NewZip()
	z.AddBytes(cfgData, "config.json")
	z.AddString("some critical error occurred here", "logs/app.log")

	f, _ := os.Create(zipPath)
	z.WriteTo(f)
	f.Close()

	src := gozip.FromFilePath(zipPath)

	// Test: Search
	matches, err := gozip.Search(src, "critical error")
	if err != nil {
		t.Errorf("Search failed: %v", err)
	}
	if len(matches) != 1 || matches[0].Name() != "logs/app.log" {
		t.Errorf("Search failed to find correct file, got: %v", matches)
	}

	// Test: Exists
	exists, _ := gozip.Exists(src, "config.json")
	if !exists {
		t.Error("Exists returned false for existing file")
	}
	exists, _ = gozip.Exists(src, "missing.txt")
	if exists {
		t.Error("Exists returned true for missing file")
	}
}

func TestArchiver_Diff_And_Replace(t *testing.T) {
	// Create Base Zip
	z1 := gozip.NewZip()
	z1.AddString("v1", "config.txt")
	z1.AddString("static", "image.png")

	buf1 := new(bytes.Buffer)
	w, err := gozip.ToWriter(buf1).Create()
	if err != nil {
		t.Errorf("Writer creation failed: %v", err)
	}
	z1.WriteTo(w) // Ignore error for brevity in test

	// Create Modified Zip
	z2 := gozip.NewZip()
	z2.AddString("v2", "config.txt")    // Modified
	z2.AddString("static", "image.png") // Same
	z2.AddString("new", "readme.md")    // Added

	buf2 := new(bytes.Buffer)
	w, err = gozip.ToWriter(buf2).Create()
	if err != nil {
		t.Errorf("Writer creation failed: %v", err)
	}
	z2.WriteTo(w)

	// Test Diff
	srcA := gozip.FromReaderAt(bytes.NewReader(buf1.Bytes()), int64(buf1.Len()))
	srcB := gozip.FromReaderAt(bytes.NewReader(buf2.Bytes()), int64(buf2.Len()))

	diff, err := gozip.Diff(srcA, srcB)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}

	if len(diff.Modified) != 1 || diff.Modified[0] != "config.txt" {
		t.Errorf("Diff Modified mismatch: %v", diff.Modified)
	}
	if len(diff.Added) != 1 || diff.Added[0] != "readme.md" {
		t.Errorf("Diff Added mismatch: %v", diff.Added)
	}

	// Test ReplaceFile
	// Replace "image.png" in buf1 with new content
	outBuf := new(bytes.Buffer)
	newImg := strings.NewReader("new_image_data")

	// Reset reader for srcA
	srcA = gozip.FromReaderAt(bytes.NewReader(buf1.Bytes()), int64(buf1.Len()))

	err = gozip.ReplaceFile(srcA, gozip.ToWriter(outBuf), "image.png", newImg)
	if err != nil {
		t.Fatalf("ReplaceFile failed: %v", err)
	}

	// Verify replacement
	resultSrc := gozip.FromReaderAt(bytes.NewReader(outBuf.Bytes()), int64(outBuf.Len()))
	data, _ := gozip.ReadFile(resultSrc, "image.png")
	if string(data) != "new_image_data" {
		t.Errorf("ReplaceFile didn't update content")
	}
	// Verify other file remained untouched
	data, _ = gozip.ReadFile(resultSrc, "config.txt")
	if string(data) != "v1" {
		t.Errorf("ReplaceFile corrupted other files")
	}
}

func TestFromURL_Integration(t *testing.T) {
	// Create a real zip file
	tmpDir := createTempDir(t)
	zipPath := filepath.Join(tmpDir, "server.zip")

	z := gozip.NewZip()
	z.AddString("remote content", "data.txt")
	// Add a dummy file to ensure offsets are non-zero
	z.AddString("padding", "000.dat")

	f, _ := os.Create(zipPath)
	z.WriteTo(f)
	f.Close()

	// Start HTTP Server supporting Range requests
	// http.ServeFile automatically handles Range headers
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, zipPath)
	}))
	defer server.Close()

	// Use FromURL
	url := server.URL + "/server.zip"
	src := gozip.FromURL(url, nil)

	// Read file
	// This should trigger HEAD -> GET (Range) -> Decompress
	data, err := gozip.ReadFile(src, "data.txt")
	if err != nil {
		t.Fatalf("ReadFile from URL failed: %v", err)
	}

	if string(data) != "remote content" {
		t.Errorf("Content mismatch: %s", string(data))
	}

	// Test Inspect
	entries, err := gozip.GetEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(entries))
	}
}

func TestUnzipToTemp(t *testing.T) {
	// Setup
	z := gozip.NewZip()
	z.AddString("temp data", "temp.txt")

	buf := new(bytes.Buffer)
	z.WriteTo(buf)

	src := gozip.FromReaderAt(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	// Test
	path, cleanup, err := gozip.UnzipToTemp(src, "test-prefix-")
	if err != nil {
		t.Fatalf("UnzipToTemp failed: %v", err)
	}
	defer cleanup()

	// Verify exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("Temp dir not created")
	}
	content, _ := os.ReadFile(filepath.Join(path, "temp.txt"))
	if string(content) != "temp data" {
		t.Error("Content mismatch")
	}

	// Verify cleanup
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Cleanup failed to remove dir")
	}
}

func TestTree(t *testing.T) {
	z := gozip.NewZip()
	z.AddString("", "a/b/c.txt")
	z.AddString("", "a/file.txt")

	buf := new(bytes.Buffer)
	z.WriteTo(buf)

	src := gozip.FromReaderAt(bytes.NewReader(buf.Bytes()), int64(buf.Len()))

	tree, err := gozip.Tree(src)
	if err != nil {
		t.Fatal(err)
	}

	// We expect "├──" and names
	expectedSubstrings := []string{"a", "b", "c.txt", "file.txt", "└──", "├──"}
	for _, s := range expectedSubstrings {
		if !contains(tree, s) {
			t.Errorf("Tree output missing substring '%s'. Output:\n%s", s, tree)
		}
	}
}

// Utility for strings.Contains in tests
func contains(s, substr string) bool {
	return len(s) >= len(substr) && func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}()
}
