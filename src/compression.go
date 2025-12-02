// Copyright 2012 The Go Authors. All rights reserved.
// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"compress/flate"
	"io"
	"sync"
)

// Compressor defines a strategy for compressing data.
// See [DeflateCompressor] as an example.
type Compressor interface {
	// Compress reads from src, compresses the data, and writes to dest.
	// Returns the number of uncompressed bytes read from src.
	Compress(src io.Reader, dest io.Writer) (int64, error)
}

// Decompressor defines a strategy for decompressing data.
// See [DeflateDecompressor] as an example.
type Decompressor interface {
	// Decompress returns a ReadCloser that reads uncompressed data from src.
	// src is the stream of compressed data.
	Decompress(src io.Reader) (io.ReadCloser, error)
}

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
