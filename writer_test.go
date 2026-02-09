// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func defaultTime() time.Time {
	return time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
}

func TestZipWriter_Encryption_Seeker(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)
	data := []byte("secret data")

	src := bytes.NewReader(data)
	cfg := FileConfig{
		EncryptionMethod:  AES256,
		Password:          "password",
		CompressionMethod: Deflate,
	}

	stats, err := zw.encodeTo(src, io.Discard, cfg)
	if err != nil {
		t.Fatalf("encodeTo encrypted seeker failed: %v", err)
	}

	if stats.uncompressedSize != int64(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), stats.uncompressedSize)
	}
	if stats.crc32 != 0 {
		t.Errorf("Expected CRC32 to be 0 for AES, got %x", stats.crc32)
	}
}

func TestZipWriter_Encryption_Stream(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)
	data := []byte("stream secret")

	src := bytes.NewBuffer(data)
	cfg := FileConfig{
		EncryptionMethod:  ZipCrypto,
		Password:          "password",
		CompressionMethod: Store,
	}

	stats, err := zw.encodeTo(src, io.Discard, cfg)
	if err != nil {
		t.Fatalf("encodeTo encrypted stream failed: %v", err)
	}

	if stats.uncompressedSize != int64(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), stats.uncompressedSize)
	}
}

func TestZipWriter_Zip64_Finalization(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := newZipWriter(ZipConfig{}, nil, buf)

	zw.entriesNum = StandardEntriesLimit + 1
	zw.headerOffset = 100
	zw.centralDirSize = 200

	err := zw.WriteCentralDirAndEndRecords()
	if err != nil {
		t.Fatalf("WriteCentralDirAndEndRecords Zip64 failed: %v", err)
	}

	out := buf.Bytes()
	if !bytes.Contains(out, []byte{0x50, 0x4b, 0x06, 0x06}) {
		t.Error("Output should contain Zip64 EOCD signature")
	}
}

func TestZipWriter_EncodeToAndUpdate_RawCopy(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	zw := newZipWriter(ZipConfig{}, nil, mw)

	rawData := []byte("already compressed data")

	f := &File{
		name:      "raw.bin",
		srcConfig: FileConfig{CompressionMethod: Deflate},
		config:    FileConfig{CompressionMethod: Deflate},
		srcFunc: func() (*io.SectionReader, error) {
			return io.NewSectionReader(bytes.NewReader(rawData), 0, int64(len(rawData))), nil
		},
	}

	snap := f.Snapshot()
	err := zw.encodeToAndUpdate(snap, mw)
	if err != nil {
		t.Fatalf("Raw copy failed: %v", err)
	}
}

func TestZipWriter_ImplicitDirs(t *testing.T) {
	t.Run("Ignore Implicit", func(t *testing.T) {
		mw := NewMemoryWriteSeeker()
		zw := newZipWriter(ZipConfig{IncludeImplicitDirs: false}, nil, mw)

		snap := &FileSnapshot{IsImplicit: true, Name: "ignored/"}
		err := zw.writeFileHeader(snap)
		if err != nil {
			t.Fatal(err)
		}
		if mw.pos > 0 {
			t.Error("Implicit dir should not be written to header")
		}
	})

	t.Run("Include Implicit", func(t *testing.T) {
		mw := NewMemoryWriteSeeker()
		zw := newZipWriter(ZipConfig{IncludeImplicitDirs: true}, nil, mw)

		snap := &FileSnapshot{IsImplicit: true, Name: "included/"}
		err := zw.writeFileHeader(snap)
		if err != nil {
			t.Fatal(err)
		}
		if mw.pos == 0 {
			t.Error("Implicit dir should be written when IncludeImplicitDirs is true")
		}
	})
}

func TestZipWriter_ResolveCompressor_Caching(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)

	c1, err := zw.resolveCompressor(Deflate, 5)
	if err != nil {
		t.Fatal(err)
	}

	c2, err := zw.resolveCompressor(Deflate, 5)
	if err != nil {
		t.Fatal(err)
	}

	if c1 != c2 {
		t.Error("Compressor should be cached and returned as the same instance")
	}

	c3, _ := zw.resolveCompressor(Deflate, 1)
	if c1 == c3 {
		t.Error("Compressors with different levels should not be the same instance")
	}
}

func TestZipWriter_UnsupportedAlgorithm(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)
	_, err := zw.resolveCompressor(CompressionMethod(999), 0)
	if !errors.Is(err, ErrAlgorithm) {
		t.Errorf("Expected ErrAlgorithm, got %v", err)
	}
}

func TestZipWriter_DataDescriptor_Streaming(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := newZipWriter(ZipConfig{}, nil, buf)

	data := []byte("data for descriptor")
	file := &File{
		name:             "stream.txt",
		uncompressedSize: int64(len(data)),
		config:           FileConfig{CompressionMethod: Store},
		openFunc: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}

	err := zw.WriteFile(file)
	if err != nil {
		t.Fatal(err)
	}

	err = zw.WriteCentralDirAndEndRecords()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(buf.Bytes(), []byte{0x50, 0x4b, 0x07, 0x08}) {
		t.Error("Stream mode without seeker should write Data Descriptor")
	}
}

func TestZipWriter_EncodeToAndUpdate_Errors(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)

	f := &File{
		name: "error.txt",
		openFunc: func() (io.ReadCloser, error) {
			return nil, errors.New("open error")
		},
	}

	err := zw.encodeToAndUpdate(f.Snapshot(), io.Discard)
	if err == nil || err.Error() != "open error" {
		t.Errorf("Expected open error, got %v", err)
	}
}

func TestZipWriter_FinalizeStats(t *testing.T) {
	zw := newZipWriter(ZipConfig{}, nil, io.Discard)
	stats := encodingStats{crc32: 0x1234}

	s1 := zw.finalizeStats(stats, FileConfig{EncryptionMethod: AES256})
	if s1.crc32 != 0 {
		t.Error("AES stats must have 0 CRC32")
	}

	s2 := zw.finalizeStats(stats, FileConfig{EncryptionMethod: ZipCrypto})
	if s2.crc32 != 0x1234 {
		t.Error("ZipCrypto stats must preserve CRC32")
	}
}

// TestParallelZipWriter_Integration verifies the full cycle with standard library Reader
func TestParallelZipWriter_Integration(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	config := ZipConfig{CompressionMethod: Deflate}

	filesCount := 5
	files := make([]*File, filesCount)
	content := "Parallel test data content"

	for i := range filesCount {
		name := fmt.Sprintf("file_%d.txt", i)
		files[i] = &File{
			name:             name,
			uncompressedSize: int64(len(content)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Deflate, CompressionLevel: DeflateNormal},
			openFunc: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(content)), nil
			},
		}
	}

	zw := newZipWriter(config, nil, mw)
	pzw := newParallelZipWriter(zw, 2)

	errs := pzw.WriteFiles(context.Background(), files)
	if len(errs) > 0 {
		t.Fatalf("WriteFiles returned errors: %v", errs)
	}

	if err := pzw.zw.WriteCentralDirAndEndRecords(); err != nil {
		t.Fatalf("WriteCentralDirAndEndRecords failed: %v", err)
	}

	// Verify with standard library zip reader
	buf := bytes.NewReader(mw.Bytes())
	r, err := zip.NewReader(buf, int64(buf.Len()))
	if err != nil {
		t.Fatalf("Standard Zip Reader failed to open archive: %v", err)
	}

	if len(r.File) != filesCount {
		t.Errorf("Expected %d files, got %d", filesCount, len(r.File))
	}

	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Errorf("Failed to open file %s: %v", f.Name, err)
			continue
		}
		data, _ := io.ReadAll(rc)
		rc.Close()

		if string(data) != content {
			t.Errorf("File %s content mismatch", f.Name)
		}
	}
}

func TestParallelZipWriter_MemoryVsDisk(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	config := ZipConfig{CompressionMethod: Store}

	smallContent := "small"
	largeContent := "larger_data"

	files := []*File{
		{
			name:             "memory_file.txt",
			uncompressedSize: int64(len(smallContent)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Store},
			openFunc:         func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(smallContent)), nil },
		},
		{
			name:             "disk_file.txt",
			uncompressedSize: int64(len(largeContent)),
			modTime:          defaultTime(),
			config:           FileConfig{CompressionMethod: Store},
			openFunc:         func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(largeContent)), nil },
		},
	}

	zw := newZipWriter(config, nil, mw)
	pzw := newParallelZipWriter(zw, 1)
	pzw.memoryThreshold = 10 // Force second file to disk

	errs := pzw.WriteFiles(context.Background(), files)
	if len(errs) > 0 {
		t.Fatalf("WriteFiles errors: %v", errs)
	}

	if err := pzw.zw.WriteCentralDirAndEndRecords(); err != nil {
		t.Fatal(err)
	}

	output := mw.Bytes()
	if !bytes.Contains(output, []byte(smallContent)) {
		t.Error("Small file content missing")
	}
	if !bytes.Contains(output, []byte(largeContent)) {
		t.Error("Large file content missing")
	}
}

func TestParallelZipWriter_ErrorHandling(t *testing.T) {
	mw := NewMemoryWriteSeeker()
	zw := newZipWriter(ZipConfig{}, nil, mw)
	pzw := newParallelZipWriter(zw, 2)

	expectedErr := errors.New("simulated open error")
	files := []*File{
		{
			name:             "bad_file.txt",
			uncompressedSize: SizeUnknown,
			openFunc: func() (io.ReadCloser, error) {
				return nil, expectedErr
			},
		},
	}

	errs := pzw.WriteFiles(context.Background(), files)
	if len(errs) == 0 {
		t.Fatal("Expected error, got none")
	}

	found := false
	for _, err := range errs {
		if strings.Contains(err.Error(), expectedErr.Error()) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected error containing %q, got %v", expectedErr, errs)
	}
}

// TestMemoryBuffer_ReadWriteSeek verifies custom buffer logic
func TestMemoryBuffer_ReadWriteSeek(t *testing.T) {
	mb := newMemoryBuffer(10)
	data := []byte("hello world")

	// Write
	n, err := mb.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Short write: %d", n)
	}

	// Seek Start
	pos, err := mb.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Errorf("Seek start failed: %v, %d", err, pos)
	}

	// Read
	readBuf := make([]byte, len(data))
	readN, err := mb.Read(readBuf)
	if err != io.EOF && err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if readN != len(data) {
		t.Errorf("Short read: %d", readN)
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("Data mismatch")
	}

	// Seek End
	pos, err = mb.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos != int64(len(data)) {
		t.Errorf("Seek end wrong pos: %d", pos)
	}

	// Reset
	mb.Reset()
	pos, _ = mb.Seek(0, io.SeekCurrent)
	if pos != 0 {
		t.Error("Reset did not zero position")
	}
	n, _ = mb.Read(make([]byte, 1))
	if n != 0 {
		t.Error("Read on reset buffer should return 0 bytes")
	}

	// Close
	mb.Close()
	_, err = mb.Write([]byte("fail"))
	if err != io.ErrClosedPipe {
		t.Errorf("Expected ErrClosedPipe, got %v", err)
	}
}

func TestMemoryBuffer_LargeGrowth(t *testing.T) {
	mb := newMemoryBuffer(1)
	size := 64 * 1024
	data := make([]byte, size)

	// Faster random fill
	rng := rand.New(rand.NewSource(42))
	rng.Read(data)

	n, err := mb.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != size {
		t.Errorf("Wrote %d bytes, expected %d", n, size)
	}

	mb.Seek(0, io.SeekStart)
	readBack, err := io.ReadAll(mb)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, readBack) {
		t.Error("Read back data mismatch")
	}
}

// memoryWriteSeeker mocks io.WriteSeeker for testing
type memoryWriteSeeker struct {
	buf []byte
	pos int64
}

func NewMemoryWriteSeeker() *memoryWriteSeeker {
	return &memoryWriteSeeker{
		buf: make([]byte, 0),
		pos: 0,
	}
}

func (m *memoryWriteSeeker) Write(p []byte) (n int, err error) {
	minCap := int(m.pos) + len(p)
	if minCap > cap(m.buf) {
		newBuf := make([]byte, len(m.buf), minCap*2)
		copy(newBuf, m.buf)
		m.buf = newBuf
	}
	if minCap > len(m.buf) {
		m.buf = m.buf[:minCap]
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memoryWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = m.pos + offset
	case io.SeekEnd:
		newPos = int64(len(m.buf)) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if newPos < 0 {
		return 0, errors.New("negative position")
	}
	m.pos = newPos
	return newPos, nil
}

func (m *memoryWriteSeeker) Bytes() []byte {
	return m.buf
}
