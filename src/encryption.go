package gozip

import (
	"crypto/rand"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

var ErrPasswordMismatch = errors.New("invalid password")

type zipCryptoWriter struct {
	dest   io.Writer
	cipher *zipCipher
}

func NewZipCryptoWriter(dest io.Writer, password string, checkByte byte) (io.WriteCloser, error) {
	cipher := newZipCipher(password)

	header := make([]byte, 12)
	if _, err := rand.Read(header); err != nil {
		return nil, fmt.Errorf("crypto rand failed: %w", err)
	}

	header[11] = checkByte
	cipher.Encrypt(header)

	if _, err := dest.Write(header); err != nil {
		return nil, fmt.Errorf("write crypto header: %w", err)
	}

	return &zipCryptoWriter{
		dest:   dest,
		cipher: cipher,
	}, nil
}

func (w *zipCryptoWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	buf := make([]byte, len(p))
	copy(buf, p)

	w.cipher.Encrypt(buf)

	return w.dest.Write(buf)
}

func (w *zipCryptoWriter) Close() error {
	if c, ok := w.dest.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type zipCryptoReader struct {
	source io.Reader
	cipher *zipCipher
}

func NewZipCryptoReader(src io.Reader, password string, flags uint16,
	                    fileCRC uint32, modTime time.Time) (io.Reader, error) {
	cipher := newZipCipher(password)

	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, fmt.Errorf("read crypto header: %w", err)
	}
	cipher.Decrypt(header)

	var expectedByte byte
	if flags&0x0008 != 0 {
		_, dosTime := timeToMsDos(modTime)
		expectedByte = byte(dosTime >> 8)
	} else {
		expectedByte = byte(fileCRC >> 24)
	}

	if header[11] != expectedByte {
		return nil, ErrPasswordMismatch
	}

	return &zipCryptoReader{
		source: src,
		cipher: cipher,
	}, nil
}

func (r *zipCryptoReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.cipher.Decrypt(p[:n])
	}
	return n, err
}

const cipherMagic = 134775813

type zipCipher struct {
	k0, k1, k2 uint32
}

func newZipCipher(password string) *zipCipher {
	z := &zipCipher{
		k0: 0x12345678,
		k1: 0x23456789,
		k2: 0x34567890,
	}
	for i := 0; i < len(password); i++ {
		z.updateKeys(password[i])
	}
	return z
}

func (z *zipCipher) updateKeys(b byte) {
	// Key0: crc32(key0, b)
	z.k0 = crc32.IEEETable[(z.k0^uint32(b))&0xff] ^ (z.k0 >> 8)

	// Key1: (key1 + (key0 & 0xff)) * 134775813 + 1
	z.k1 = z.k1 + (z.k0 & 0xff)
	z.k1 = z.k1*cipherMagic + 1

	// Key2: crc32(key2, key1 >> 24)
	z.k2 = crc32.IEEETable[(z.k2^uint32(byte(z.k1>>24)))&0xff] ^ (z.k2 >> 8)
}

func (z *zipCipher) magicByte() byte {
	t := z.k2 | 2
	return byte((t * (t ^ 1)) >> 8)
}

func (z *zipCipher) Encrypt(buf []byte) {
	for i, b := range buf {
		c := b ^ z.magicByte()
		z.updateKeys(b)
		buf[i] = c
	}
}

func (z *zipCipher) Decrypt(buf []byte) {
	for i, c := range buf {
		k := z.magicByte()
		b := c ^ k
		z.updateKeys(b)
		buf[i] = b
	}
}