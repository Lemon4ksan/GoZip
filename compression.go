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

// Supported compression methods according to ZIP specification.
// Note: This library natively supports Store (0) and Deflate (8).
// Other methods require registering custom compressors via RegisterCompressor.
const (
	Store     CompressionMethod = 0  // No compression - file stored as-is
	Deflate   CompressionMethod = 8  // DEFLATE compression (most common)
	Deflate64 CompressionMethod = 9  // DEFLATE64(tm) - Not supported natively
	BZIP2     CompressionMethod = 12 // BZIP2 - Not supported natively
	LZMA      CompressionMethod = 14 // LZMA - Not supported natively
	ZStandard CompressionMethod = 93 // Zstandard - Not supported natively
)

// Compression levels for DEFLATE algorithm.
const (
	DeflateNormal    = flate.DefaultCompression // -1
	DeflateMaximum   = flate.BestCompression    // 9
	DeflateFast      = 3
	DeflateSuperFast = flate.BestSpeed     // 1 (Same as Fast in stdlib)
	DeflateStore     = flate.NoCompression // 0
)

// StoredCompressor implements no compression (STORE method).
type StoredCompressor struct{}

func (sc *StoredCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	// io.Copy uses io.WriterTo if available, making this efficient
	return io.Copy(dest, src)
}

// DeflateCompressor implements DEFLATE compression with memory pooling.
type DeflateCompressor struct {
	writers sync.Pool
	buffers sync.Pool
}

// NewDeflateCompressor creates a reusable compressor for a specific level.
func NewDeflateCompressor(level int) *DeflateCompressor {
	if level < flate.HuffmanOnly || level > flate.BestCompression {
		level = flate.DefaultCompression
	}

	return &DeflateCompressor{
		writers: sync.Pool{
			New: func() interface{} {
				// We use io.Discard just to initialize the struct structure.
				// The error is safe to ignore here because we validated the level above.
				w, _ := flate.NewWriter(io.Discard, level)
				return w
			},
		},
		buffers: sync.Pool{
			New: func() interface{} {
				// 32KB is a standard efficient buffer size for io.CopyBuffer
				b := make([]byte, 32*1024)
				return &b
			},
		},
	}
}

func (d *DeflateCompressor) Compress(src io.Reader, dest io.Writer) (int64, error) {
	w := d.writers.Get().(*flate.Writer)
	defer d.writers.Put(w)

	// Reset directs the compressor to write to the new destination
	w.Reset(dest)

	bufPtr := d.buffers.Get().(*[]byte)
	defer d.buffers.Put(bufPtr)

	// CopyBuffer avoids allocating a temporary buffer on every call
	n, err := io.CopyBuffer(w, src, *bufPtr)
	if err != nil {
		return n, err
	}

	// Close flushes any pending data and writes the DEFLATE footer.
	// It does NOT close the underlying destination writer.
	if err := w.Close(); err != nil {
		return n, err
	}

	return n, nil
}

// StoredDecompressor implements the "Store" method (no compression).
type StoredDecompressor struct{}

func (sd *StoredDecompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	if rc, ok := src.(io.ReadCloser); ok {
		return rc, nil
	}
	return io.NopCloser(src), nil
}

// DeflateDecompressor implements the "Deflate" method.
type DeflateDecompressor struct{}

func (dd *DeflateDecompressor) Decompress(src io.Reader) (io.ReadCloser, error) {
	// flate.NewReader returns an io.ReadCloser.
	// The Close() method of the returned reader does not close the underlying src.
	return flate.NewReader(src), nil
}
