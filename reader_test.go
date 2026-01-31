// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"testing"

	"github.com/lemon4ksan/gozip/internal"
	"github.com/lemon4ksan/gozip/internal/sys"
)

// writeOnlyBuffer wraps bytes.Buffer to hide Bytes(), ReadFrom and other methods,
// exposing only Write. This forces the ZipWriter to use Data Descriptors (Bit 3).
type writeOnlyBuffer struct {
	buf *bytes.Buffer
}

func (w *writeOnlyBuffer) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func makeEOCD(entries uint16, cdSize, cdOffset uint32, comment string) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, internal.EOCDSignature)
	binary.Write(buf, binary.LittleEndian, uint16(0))            // Disk number
	binary.Write(buf, binary.LittleEndian, uint16(0))            // Disk number with start
	binary.Write(buf, binary.LittleEndian, entries)              // Entries on disk
	binary.Write(buf, binary.LittleEndian, entries)              // Total entries
	binary.Write(buf, binary.LittleEndian, cdSize)               // Size of CD
	binary.Write(buf, binary.LittleEndian, cdOffset)             // Offset of CD
	binary.Write(buf, binary.LittleEndian, uint16(len(comment))) // Comment len
	buf.WriteString(comment)
	return buf.Bytes()
}

func TestFindAndReadEndOfCentralDir(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "Simple EOCD at end",
			data:      makeEOCD(5, 100, 200, ""),
			wantFound: true,
		},
		{
			name:      "EOCD with comment",
			data:      makeEOCD(1, 50, 10, "This is a comment"),
			wantFound: true,
		},
		{
			name:      "EOCD with comment preceded by garbage",
			data:      append([]byte("garbage data..."), makeEOCD(1, 50, 10, "Comment")...),
			wantFound: true,
		},
		{
			name:      "Fake EOCD signature in comment (edge case)",
			data:      append([]byte("prefix"), makeEOCD(1, 50, 10, "Fake PK\x05\x06 signature")...),
			wantFound: true,
		},
		{
			name:      "File too small",
			data:      []byte("too short"),
			wantFound: false,
			wantErr:   true,
		},
		{
			name:      "No EOCD signature",
			data:      make([]byte, 100), // Just zeros
			wantFound: false,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewReader(tt.data)
			zr := newZipReader(r, r.Size(), nil, ZipConfig{})

			got, err := zr.FindAndReadEOCD(context.Background())

			if (err != nil) != tt.wantErr {
				t.Fatalf("findAndReadEndOfCentralDir() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tt.wantFound {
				if got.EntriesNum == 0 && len(tt.data) > 30 {
					// Basic check to see if we parsed something meaningful from our helper
					// (assuming helper sets entries > 0 usually, except specific cases)
				}
			}
		})
	}
}

func TestFindEOCD_BufferBoundary(t *testing.T) {
	comment := "short"
	eocd := makeEOCD(1, 10, 10, comment)

	// Ensure EOCD straddles the 1024-byte buffer boundary used in scanning
	prefixLen := 1024 + 10
	data := make([]byte, prefixLen)
	data = append(data, eocd...)

	r := bytes.NewReader(data)
	zr := newZipReader(r, r.Size(), nil, ZipConfig{})

	res, err := zr.FindAndReadEOCD(context.Background())
	if err != nil {
		t.Fatalf("Failed to find EOCD across buffer boundaries: %v", err)
	}
	if res.CommentLength != uint16(len(comment)) {
		t.Errorf("Wrong comment length found: %d", res.CommentLength)
	}
}

func TestNewFileFromCentralDir_Zip64(t *testing.T) {
	cd := internal.CentralDirectory{
		UncompressedSize:  StandardSizeLimit,
		CompressedSize:    StandardSizeLimit,
		LocalHeaderOffset: StandardSizeLimit,
		Filename:          "large_file.dat",
		ExtraField:        make([]byte, 0),
	}

	// Construct the Zip64 Extra Field payload
	// The map value in internal.CentralDirectory stores only the data payload.
	extraPayload := new(bytes.Buffer)
	binary.Write(extraPayload, binary.LittleEndian, []byte{0x01, 0x00, 0x18, 0x00})
	binary.Write(extraPayload, binary.LittleEndian, uint64(5000000000)) // Real Uncompressed > 4GB
	binary.Write(extraPayload, binary.LittleEndian, uint64(4000000000)) // Real Compressed
	binary.Write(extraPayload, binary.LittleEndian, uint64(1000000000)) // Real Offset

	cd.ExtraField = extraPayload.Bytes()

	zr := &zipReader{}
	f := zr.newFileFromCentralDir(cd)

	if f.uncompressedSize != 5000000000 {
		t.Errorf("Zip64 uncompressed size mismatch: got %d", f.uncompressedSize)
	}
	if f.compressedSize != 4000000000 {
		t.Errorf("Zip64 compressed size mismatch: got %d", f.compressedSize)
	}
	if f.localHeaderOffset != 1000000000 {
		t.Errorf("Zip64 offset mismatch: got %d", f.localHeaderOffset)
	}
}

func TestParseFileExternalAttributes(t *testing.T) {
	tests := []struct {
		name     string
		entry    internal.CentralDirectory
		wantMode fs.FileMode
	}{
		{
			name: "Unix Regular File (0644)",
			entry: internal.CentralDirectory{
				VersionMadeBy:          uint16(sys.HostSystemUNIX) << 8,
				ExternalFileAttributes: uint32(0644) << 16,
			},
			wantMode: 0644,
		},
		{
			name: "Unix Directory (0755)",
			entry: internal.CentralDirectory{
				VersionMadeBy:          uint16(sys.HostSystemUNIX) << 8,
				ExternalFileAttributes: uint32(0040755) << 16, // IFDIR + 0755
			},
			wantMode: 0755 | fs.ModeDir,
		},
		{
			name: "Windows/DOS ReadOnly",
			entry: internal.CentralDirectory{
				VersionMadeBy:          uint16(sys.HostSystemFAT) << 8,
				ExternalFileAttributes: 0x01, // ReadOnly bit
				Filename:               "file.txt",
			},
			// 0644 &^ 0222 = 0444
			wantMode: 0444,
		},
		{
			name: "Windows Directory",
			entry: internal.CentralDirectory{
				VersionMadeBy:          uint16(sys.HostSystemFAT) << 8,
				ExternalFileAttributes: 0x10, // Directory bit
				Filename:               "folder/",
			},
			wantMode: 0755 | fs.ModeDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := internal.ParseFileMode(tt.entry)
			if got != tt.wantMode {
				t.Errorf("parseFileExternalAttributes() = %v, want %v", got, tt.wantMode)
			}
		})
	}
}

func TestChecksumReader(t *testing.T) {
	data := []byte("hello world")
	crc := crc32.ChecksumIEEE(data)

	t.Run("Valid Checksum", func(t *testing.T) {
		rc := io.NopCloser(bytes.NewReader(data))
		cr := &checksumReader{
			rc:       rc,
			hash:     crc32.NewIEEE(),
			wantCRC:  crc,
			wantSize: uint64(len(data)),
		}

		if _, err := io.Copy(io.Discard, cr); err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if err := cr.Close(); err != nil {
			t.Errorf("Close failed (checksum valid): %v", err)
		}
	})

	t.Run("Partial Read (No Error)", func(t *testing.T) {
		rc := io.NopCloser(bytes.NewReader(data))
		cr := &checksumReader{
			rc:       rc,
			hash:     crc32.NewIEEE(),
			wantCRC:  crc,
			wantSize: uint64(len(data)),
		}

		// Read only 1 byte
		if _, err := io.ReadFull(cr, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}

		// Closing partial read should NOT return error (allows peeking)
		if err := cr.Close(); err != nil {
			t.Errorf("Expected nil error for partial read close, got: %v", err)
		}
	})

	t.Run("Invalid Checksum", func(t *testing.T) {
		rc := io.NopCloser(bytes.NewReader([]byte("wrong data")))
		cr := &checksumReader{
			rc:       rc,
			hash:     crc32.NewIEEE(),
			wantCRC:  crc,
			wantSize: uint64(len("wrong data")),
		}

		io.Copy(io.Discard, cr)
		err := cr.Close()
		if !errors.Is(err, ErrChecksum) {
			t.Errorf("Expected ErrCheckSum, got: %v", err)
		}
	})

	t.Run("Size Mismatch (Too long)", func(t *testing.T) {
		longData := append(data, '!')
		rc := io.NopCloser(bytes.NewReader(longData))
		cr := &checksumReader{
			rc:       rc,
			hash:     crc32.NewIEEE(),
			wantCRC:  crc,
			wantSize: uint64(len(data)),
		}

		_, err := io.Copy(io.Discard, cr)
		if !errors.Is(err, ErrSizeMismatch) {
			t.Errorf("Expected ErrSizeMismatch, got %v", err)
		}
	})
}

func TestZipReader_OpenFile_Integration(t *testing.T) {
	buf := new(bytes.Buffer)

	content := []byte("test content")
	crc := crc32.ChecksumIEEE(content)

	lhOffset := buf.Len()
	binary.Write(buf, binary.LittleEndian, internal.LocalFileHeaderSignature)
	binary.Write(buf, binary.LittleEndian, uint16(20))    // Version
	binary.Write(buf, binary.LittleEndian, uint16(0))     // Flags
	binary.Write(buf, binary.LittleEndian, uint16(Store)) // Method
	binary.Write(buf, binary.LittleEndian, uint16(0))     // Time
	binary.Write(buf, binary.LittleEndian, uint16(0))     // Date
	binary.Write(buf, binary.LittleEndian, crc)
	binary.Write(buf, binary.LittleEndian, uint32(len(content))) // Compressed
	binary.Write(buf, binary.LittleEndian, uint32(len(content))) // Uncompressed
	binary.Write(buf, binary.LittleEndian, uint16(4))            // Filename Len
	binary.Write(buf, binary.LittleEndian, uint16(0))            // Extra Len
	buf.WriteString("test")

	buf.Write(content)

	reader := bytes.NewReader(buf.Bytes())
	zr := newZipReader(reader, reader.Size(), nil, ZipConfig{})

	f := &File{
		name:              "test",
		config:            FileConfig{CompressionMethod: Store},
		localHeaderOffset: int64(lhOffset),
		compressedSize:    int64(len(content)),
		uncompressedSize:  int64(len(content)),
		crc32:             crc,
	}

	rc, err := zr.openFile(f)
	if err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer rc.Close()

	readBuf, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if !bytes.Equal(readBuf, content) {
		t.Errorf("Content mismatch: got %s, want %s", readBuf, content)
	}

	if err := rc.Close(); err != nil {
		t.Errorf("Close (checksum verification) failed: %v", err)
	}
}

func TestStreamReader_RoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)
	archive := NewZip()
	archive.SetConfig(ZipConfig{
		UseImplicitDirs: true,
	})

	testFiles := map[string]string{
		"hello.txt":       "Hello World",
		"dir/nested.dat":  "Nested Data",
		"images/logo.png": string(make([]byte, 5000)),
	}

	for name, content := range testFiles {
		if err := archive.AddString(content, name); err != nil {
			t.Fatalf("AddString failed: %v", err)
		}
	}

	if _, err := archive.WriteTo(buf); err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	sr := NewStreamReader(bytes.NewReader(buf.Bytes()))

	filesFound := 0
	for {
		f, err := sr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next() failed: %v", err)
		}

		expectedContent, ok := testFiles[f.Name()]
		if !ok {
			t.Errorf("Unexpected file found: %s", f.Name())
			continue
		}

		rc, err := sr.Open()
		if err != nil {
			t.Fatalf("Open() failed for %s: %v", f.Name(), err)
		}

		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		rc.Close()

		if string(content) != expectedContent {
			t.Errorf("Content mismatch for %s", f.Name())
		}
		filesFound++
	}

	if filesFound != len(testFiles) {
		t.Errorf("Expected %d files, found %d", len(testFiles), filesFound)
	}
}

func TestStreamReader_DataDescriptors(t *testing.T) {
	buf := new(bytes.Buffer)
	dest := &writeOnlyBuffer{buf: buf}

	archive := NewZip()

	archive.AddString("compressed data string repeated repeated", "deflate.txt",
		WithCompression(Deflate, DeflateNormal))

	archive.AddString("stored data string", "store.txt",
		WithCompression(Store, 0))

	if _, err := archive.WriteTo(dest); err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	sr := NewStreamReader(buf)

	f1, err := sr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f1.Name() != "deflate.txt" {
		t.Fatalf("Expected deflate.txt, got %s", f1.Name())
	}
	if f1.flags&0x8 == 0 {
		t.Fatal("Bit 3 (Data Descriptor) should be set for streaming write")
	}
	rc1, _ := sr.Open()
	data1, _ := io.ReadAll(rc1)
	if string(data1) != "compressed data string repeated repeated" {
		t.Error("Deflate content mismatch")
	}

	f2, err := sr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f2.Name() != "store.txt" {
		t.Fatalf("Expected store.txt, got %s", f2.Name())
	}
	rc2, _ := sr.Open()
	data2, _ := io.ReadAll(rc2)
	if string(data2) != "stored data string" {
		t.Error("Store content mismatch")
	}

	if _, err := sr.Next(); err != io.EOF {
		t.Error("Expected EOF")
	}
}

func TestStreamReader_SkipAndPartial(t *testing.T) {
	buf := new(bytes.Buffer)
	archive := NewZip()

	archive.AddString("file1 content", "1.txt")
	archive.AddString("file2 content is longer", "2.txt")
	archive.AddString("file3 content", "3.txt")

	archive.WriteTo(buf)

	sr := NewStreamReader(buf)

	f1, _ := sr.Next()
	if f1.Name() != "1.txt" {
		t.Fatal("Order mismatch")
	}
	rc, _ := sr.Open()
	io.ReadAll(rc)
	rc.Close()

	f2, err := sr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f2.Name() != "2.txt" {
		t.Fatal("Order mismatch 2")
	}

	f3, err := sr.Next()
	if err != nil {
		t.Fatalf("Failed to skip file 2: %v", err)
	}
	if f3.Name() != "3.txt" {
		t.Fatal("Order mismatch 3")
	}

	rc3, _ := sr.Open()
	p := make([]byte, 5)
	rc3.Read(p)

	_, err = sr.Next()
	if err != io.EOF {
		t.Errorf("Expected EOF, got %v", err)
	}
}
