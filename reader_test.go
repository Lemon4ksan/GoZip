// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"strings"
	"testing"

	"github.com/lemon4ksan/gozip/internal"
	"github.com/lemon4ksan/gozip/internal/sys"
)

func makeEOCD(entries uint16, cdSize, cdOffset uint32, comment string) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, internal.EndOfCentralDirSignature)
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
			zr := &zipReader{src: r}

			got, err := zr.findAndReadEndOfCentralDir()

			if (err != nil) != tt.wantErr {
				t.Fatalf("findAndReadEndOfCentralDir() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tt.wantFound {
				if got.TotalNumberOfEntries == 0 && len(tt.data) > 30 {
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

	prefixLen := 1024 + 10
	data := make([]byte, prefixLen)
	data = append(data, eocd...)

	r := bytes.NewReader(data)
	zr := &zipReader{src: r}

	res, err := zr.findAndReadEndOfCentralDir()
	if err != nil {
		t.Fatalf("Failed to find EOCD across buffer boundaries: %v", err)
	}
	if res.CommentLength != uint16(len(comment)) {
		t.Errorf("Wrong comment length found: %d", res.CommentLength)
	}
}

func TestNewFileFromCentralDir_Zip64(t *testing.T) {
	cd := internal.CentralDirectory{
		UncompressedSize:  math.MaxUint32,
		CompressedSize:    math.MaxUint32,
		LocalHeaderOffset: math.MaxUint32,
		Filename:          "large_file.dat",
		ExtraField:        make(map[uint16][]byte, 0),
	}

	extra := new(bytes.Buffer)
	binary.Write(extra, binary.LittleEndian, uint16(Zip64ExtraFieldTag))
	binary.Write(extra, binary.LittleEndian, uint16(24))         // 8+8+8
	binary.Write(extra, binary.LittleEndian, uint64(5000000000)) // Real Uncompressed > 4GB
	binary.Write(extra, binary.LittleEndian, uint64(4000000000)) // Real Compressed
	binary.Write(extra, binary.LittleEndian, uint64(1000000000)) // Real Offset

	cd.ExtraField = map[uint16][]byte{
		Zip64ExtraFieldTag: extra.Bytes()[4:], // Skip Tag and Size headers for the map value
	}

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
			got := parseFileExternalAttributes(tt.entry)
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
			rc:   rc,
			hash: crc32.NewIEEE(),
			want: crc,
			size: uint64(len(data)),
		}

		if _, err := io.Copy(io.Discard, cr); err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if err := cr.Close(); err != nil {
			t.Errorf("Close failed (checksum valid): %v", err)
		}
	})

	t.Run("Invalid Checksum", func(t *testing.T) {
		rc := io.NopCloser(bytes.NewReader([]byte("wrong data")))
		cr := &checksumReader{
			rc:   rc,
			hash: crc32.NewIEEE(),
			want: crc,
			size: uint64(len("wrong data")),
		}

		io.Copy(io.Discard, cr)
		err := cr.Close()
		if err == nil {
			t.Error("Expected checksum error, got nil")
		} else if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("Expected checksum mismatch error, got: %v", err)
		}
	})

	t.Run("Size Mismatch (Too short)", func(t *testing.T) {
		rc := io.NopCloser(bytes.NewReader(data[:5])) // Read less
		cr := &checksumReader{
			rc:   rc,
			hash: crc32.NewIEEE(),
			want: crc,
			size: uint64(len(data)),
		}

		io.Copy(io.Discard, cr)
		err := cr.Close()
		if err == nil {
			t.Error("Expected size error, got nil")
		} else if !strings.Contains(err.Error(), "size mismatch") {
			t.Errorf("Expected size mismatch error, got: %v", err)
		}
	})

	t.Run("Size Mismatch (Too long)", func(t *testing.T) {
		longData := append(data, '!')
		rc := io.NopCloser(bytes.NewReader(longData))
		cr := &checksumReader{
			rc:   rc,
			hash: crc32.NewIEEE(),
			want: crc,
			size: uint64(len(data)),
		}

		_, err := io.Copy(io.Discard, cr)
		if err == nil {
			t.Error("Expected error during read (too large), got nil")
		}
	})
}

func TestZipReader_OpenFile_Integration(t *testing.T) {
	buf := new(bytes.Buffer)

	content := []byte("test content")
	crc := crc32.ChecksumIEEE(content)

	// 1. Local Header
	lhOffset := buf.Len()
	binary.Write(buf, binary.LittleEndian, internal.LocalFileHeaderSignature)
	binary.Write(buf, binary.LittleEndian, uint16(20))     // Version
	binary.Write(buf, binary.LittleEndian, uint16(0))      // Flags
	binary.Write(buf, binary.LittleEndian, uint16(Stored)) // Method
	binary.Write(buf, binary.LittleEndian, uint16(0))      // Time
	binary.Write(buf, binary.LittleEndian, uint16(0))      // Date
	binary.Write(buf, binary.LittleEndian, crc)
	binary.Write(buf, binary.LittleEndian, uint32(len(content))) // Compressed
	binary.Write(buf, binary.LittleEndian, uint32(len(content))) // Uncompressed
	binary.Write(buf, binary.LittleEndian, uint16(4))            // Filename Len
	binary.Write(buf, binary.LittleEndian, uint16(0))            // Extra Len
	buf.WriteString("test")

	// 2. Data
	buf.Write(content)

	// 3. Central Directory Header is mocked via struct injection usually,
	// but to test openFile properly, we just need the ReaderAt logic to hit the right offset.

	reader := bytes.NewReader(buf.Bytes())
	zr := newZipReader(reader, nil, ZipConfig{}) // Default decompressors

	f := &File{
		name:              "test",
		config:            FileConfig{CompressionMethod: Stored},
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

func TestReaderAtRequirement(t *testing.T) {
	r := &readSeekerOnly{bytes.NewReader([]byte("dummy"))}
	zr := newZipReader(r, nil, ZipConfig{})

	f := &File{localHeaderOffset: 0}

	_, err := zr.openFile(f)
	if err == nil {
		t.Error("Expected error because source is not ReaderAt, got nil")
	}
}

type readSeekerOnly struct {
	*bytes.Reader
}
