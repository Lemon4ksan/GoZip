// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/lemon4ksan/gozip/internal"
)

func defaultTime() time.Time {
	return time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
}

// TestZipWriter_WriteFileHeader tests local file header writing
func TestZipWriter_WriteFileHeader(t *testing.T) {
	tests := []struct {
		name    string
		file    *File
		wantErr bool
	}{
		{
			name: "basic file header",
			file: &File{
				name:    "test.txt",
				modTime: defaultTime(),
			},
			wantErr: false,
		},
		{
			name: "file with long name",
			file: &File{
				name:    strings.Repeat("a", 100) + ".txt",
				modTime: defaultTime(),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := NewMemoryWriteSeeker()
			zw := newZipWriter(ZipConfig{}, nil, mw)

			err := zw.writeFileHeader(tt.file)

			if (err != nil) != tt.wantErr {
				t.Errorf("WriteFileHeader() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify header signature
			buf := mw.Bytes()
			if len(buf) < 4 {
				t.Error("WriteFileHeader() should write at least 4 bytes")
			}

			const expectedSig = internal.LocalFileHeaderSignature
			signature := binary.LittleEndian.Uint32(buf[:4])
			if signature != expectedSig {
				t.Errorf("WriteFileHeader() signature = %x, want %x", signature, expectedSig)
			}
		})
	}
}

// TestZipWriter_EncodeToWriter tests compression logic
func TestZipWriter_EncodeToWriter(t *testing.T) {
	const testData = "This is a long text with some repetition to demonstrate compression in action"
	const expectedCRC = 0xdf3e3946

	tests := []struct {
		name        string
		compression CompressionMethod
		level       int
		wantErr     bool
	}{
		{
			name:        "store compression",
			compression: Store,
			wantErr:     false,
		},
		{
			name:        "deflate compression",
			compression: Deflate,
			level:       DeflateNormal,
			wantErr:     false,
		},
		{
			name:        "unsupported compression",
			compression: CompressionMethod(99),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var destBuf bytes.Buffer
			zw := newZipWriter(ZipConfig{}, nil, NewMemoryWriteSeeker())

			src := strings.NewReader(testData)
			config := FileConfig{
				CompressionMethod: tt.compression,
				CompressionLevel:  tt.level,
				EncryptionMethod:  NotEncrypted,
			}

			stats, err := zw.encodeToWriter(src, &destBuf, config)

			if (err != nil) != tt.wantErr {
				t.Errorf("encodeToWriter() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if stats.uncompressedSize != int64(len(testData)) {
				t.Errorf("Stats.uncompressed = %d, want %d", stats.uncompressedSize, len(testData))
			}

			if stats.crc32 != uint32(expectedCRC) {
				t.Errorf("Stats.crc32 = %d, want %d", stats.crc32, expectedCRC)
			}
		})
	}
}

// TestZipWriter_WriteFile_Strategies verify logic selection (Stream vs TempFile)
func TestZipWriter_WriteFile_Strategies(t *testing.T) {
	data := []byte("test data for strategies")

	tests := []struct {
		name             string
		uncompressedSize int64
		isDir            bool
	}{
		{
			name:             "Stream Path (Known Size)",
			uncompressedSize: int64(len(data)),
			isDir:            false,
		},
		{
			name:             "Buffered Path (Unknown Size)",
			uncompressedSize: SizeUnknown,
			isDir:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := NewMemoryWriteSeeker()
			zw := newZipWriter(ZipConfig{}, nil, mw)

			file := &File{
				name:             "test",
				uncompressedSize: tt.uncompressedSize,
				isDir:            tt.isDir,
				modTime:          defaultTime(),
				config:           FileConfig{CompressionMethod: Store},
				openFunc: func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(data)), nil
				},
			}

			err := zw.WriteFile(file)
			if err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			output := mw.Bytes()
			if len(output) == 0 {
				t.Fatal("Output is empty")
			}

			if file.crc32 == 0 {
				t.Error("File CRC32 was not updated")
			}
			if file.compressedSize != int64(len(data)) {
				t.Errorf("Compressed size mismatch: got %d want %d", file.compressedSize, len(data))
			}
		})
	}
}

// TestZipWriter_UpdateLocalHeader tests patching of CRC and sizes in Stream mode
func TestZipWriter_UpdateLocalHeader(t *testing.T) {
	file := &File{
		name:              "test.txt",
		crc32:             0x12345678,
		compressedSize:    100,
		uncompressedSize:  100,
		localHeaderOffset: 0,
		modTime:           defaultTime(),
	}

	mw := NewMemoryWriteSeeker()
	zw := newZipWriter(ZipConfig{}, nil, mw)

	// 1. Write initial header
	err := zw.writeFileHeader(file)
	if err != nil {
		t.Fatalf("WriteFileHeader() error = %v", err)
	}

	// 2. Simulate data writing
	mw.Write(make([]byte, 100))

	// 3. Update header
	err = zw.updateLocalHeader(file)
	if err != nil {
		t.Errorf("UpdateLocalHeader() error = %v", err)
	}

	// 4. Verify Patching
	headerData := mw.Bytes()

	// CRC is at offset 14 (4 bytes), CompSize at 18, UncompSize at 22
	crcFromHeader := binary.LittleEndian.Uint32(headerData[14:18])
	if crcFromHeader != file.crc32 {
		t.Errorf("CRC32 in header = %x, want %x", crcFromHeader, file.crc32)
	}

	compSize := binary.LittleEndian.Uint32(headerData[18:22])
	if compSize != uint32(file.compressedSize) {
		t.Errorf("CompressedSize in header = %d, want %d", compSize, file.compressedSize)
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

	pzw := newParallelZipWriter(config, nil, mw, 2)

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

	pzw := newParallelZipWriter(config, nil, mw, 1)
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
	pzw := newParallelZipWriter(ZipConfig{}, nil, mw, 2)

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
