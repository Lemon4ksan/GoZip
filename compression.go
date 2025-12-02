// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"compress/flate"
	"io"
	"sync"
)

// CompressionMethod represents the compression algorithm used for a file in the ZIP archive
type CompressionMethod uint16

// Supported compression methods according to ZIP specification
const (
	Stored    CompressionMethod = 0  // No compression - file stored as-is
	Deflated  CompressionMethod = 8  // DEFLATE compression (most common)
	Deflate64 CompressionMethod = 9  // DEFLATE64(tm) enhanced compression
	BZIP2     CompressionMethod = 12 // BZIP2 compression (more efficient but slower compression)
	LZMA      CompressionMethod = 14 // LZMA compression (high compression ratio)
	ZStandard CompressionMethod = 93 // Zstandard compression (fastest decompression)
)

// Compression levels for DEFLATE algorithm
const (
	DeflateNormal    = 6 // Default compression level (good balance between speed and ratio)
	DeflateMaximum   = 9 // Maximum compression (best ratio, slowest speed)
	DeflateFast      = 3 // Fast compression (lower ratio, faster speed)
	DeflateSuperFast = 1 // Super fast compression (lowest ratio, fastest speed)
)

// StoredCompressor implements no compression (STORE method)
type StoredCompressor struct{}

func (sc *StoredCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	return io.Copy(dest, src)
}

// DeflateCompressor implements DEFLATE compression with memory pooling
type DeflateCompressor struct {
	pool sync.Pool
}

// NewDeflateCompressor creates a reusable compressor for a specific level
func NewDeflateCompressor(level int) *DeflateCompressor {
	return &DeflateCompressor{
		pool: sync.Pool{
			New: func() interface{} {
				w, _ := flate.NewWriter(io.Discard, level)
				return w
			},
		},
	}
}

func (d *DeflateCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	w := d.pool.Get().(*flate.Writer)
	defer d.pool.Put(w)

	w.Reset(dest)

	n, err := io.Copy(w, src)
	if err != nil {
		return n, err
	}

	if err := w.Close(); err != nil {
		return n, err
	}

	return n, nil
}

// StoredDecompressor implements the "Store" method (no compression)
type StoredDecompressor struct{}

func (sd *StoredDecompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	if rc, ok := src.(io.ReadCloser); ok {
		return rc, nil
	}
	return io.NopCloser(src), nil
}

// DeflateDecompressor implements the "Deflate" method
type DeflateDecompressor struct{}

func (dd *DeflateDecompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	return flate.NewReader(src), nil
}
