// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// EncryptionMethod represents the encryption algorithm used for file protection.
type EncryptionMethod uint16

// Supported encryption methods
const (
	NotEncrypted EncryptionMethod = 0 // No encryption - file stored in plaintext
	ZipCrypto    EncryptionMethod = 1 // Legacy encryption. Vulnerable to brute force attacks
	AES256       EncryptionMethod = 2 // Modern AES256 encryption
)

// zipCryptoWriter implements the legacy PKWARE encryption.
type zipCryptoWriter struct {
	dest   io.Writer
	cipher *zipCipher
}

func newZipCryptoWriter(dest io.Writer, password string, checkByte byte) (io.WriteCloser, error) {
	cipher := newZipCipher(password)

	// Write the encryption header (12 bytes)
	header := make([]byte, 12)
	if _, err := rand.Read(header); err != nil {
		return nil, fmt.Errorf("crypto rand failed: %w", err)
	}

	// The last byte of the header is used as a check byte (CRC or ModTime)
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

	// Encrypt in place (on a copy to allow reusing input buffer)
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

func newZipCryptoReader(src io.Reader, password string, flags uint16, crc32Val uint32, modTime uint16) (io.Reader, error) {
	cipher := newZipCipher(password)

	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, fmt.Errorf("read crypto header: %w", err)
	}
	cipher.Decrypt(header)

	// Verify password
	var expectedByte byte
	if flags&0x8 != 0 {
		// If bit 3 is set, ModTime is used (local header doesn't have CRC)
		expectedByte = byte(modTime >> 8)
	} else {
		expectedByte = byte(crc32Val >> 24)
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

// zipCipher implements the legacy ZipCrypto algorithm.
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

// AES Constants
const (
	aes256KeySize  = 32 // 32 bytes = 256 bits
	aes256SaltSize = 16 // 16 bytes for AES-256
	aesMacSize     = 10 // HMAC-SHA1 truncated to 10 bytes
	aesPvvSize     = 2  // Password Verification Value
)

// aesWriter implements WinZip AES-256 encryption.
type aesWriter struct {
	dest   io.Writer
	stream *winZipCounter
	mac    hash.Hash
}

func newAes256Writer(dest io.Writer, password string) (io.WriteCloser, error) {
	salt := make([]byte, aes256SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("aes rand: %w", err)
	}

	keys := deriveAesKeys(password, salt)

	// Write Salt
	if _, err := dest.Write(salt); err != nil {
		return nil, fmt.Errorf("write salt: %w", err)
	}

	// Write PVV (Password Verification Value)
	if _, err := dest.Write(keys.pvv); err != nil {
		return nil, fmt.Errorf("write pvv: %w", err)
	}

	block, err := aes.NewCipher(keys.encKey)
	if err != nil {
		return nil, err
	}

	return &aesWriter{
		dest:   dest,
		stream: newWinZipCounter(block),
		mac:    hmac.New(sha1.New, keys.macKey),
	}, nil
}

func (w *aesWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// We must encrypt into a new buffer to avoid modifying p
	buf := make([]byte, len(p))
	w.stream.XORKeyStream(buf, p)

	// WinZip AES: Encrypt-then-MAC (HMAC is computed on ciphertext)
	w.mac.Write(buf)

	return w.dest.Write(buf)
}

func (w *aesWriter) Close() error {
	// Append the 10-byte authentication code
	sum := w.mac.Sum(nil)
	if _, err := w.dest.Write(sum[:aesMacSize]); err != nil {
		return fmt.Errorf("write auth code: %w", err)
	}

	if c, ok := w.dest.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type aesReader struct {
	limitR     io.Reader
	macSrc     io.Reader
	stream     *winZipCounter
	mac        hash.Hash
	checkOnEOF bool
}

// newAes256Reader creates a reader that handles WinZip AES decryption.
// src MUST be positioned at the start of the AES data (Salt).
// compressedSize is the size of the data segment in the ZIP file (including Salt/PVV/MAC).
func newAes256Reader(src io.Reader, password string, compressedSize int64) (io.Reader, error) {
	salt := make([]byte, aes256SaltSize)
	if _, err := io.ReadFull(src, salt); err != nil {
		return nil, fmt.Errorf("read salt: %w", err)
	}

	keys := deriveAesKeys(password, salt)
	pvv := make([]byte, aesPvvSize)
	if _, err := io.ReadFull(src, pvv); err != nil {
		return nil, fmt.Errorf("read pvv: %w", err)
	}
	if !bytes.Equal(pvv, keys.pvv) {
		return nil, ErrPasswordMismatch
	}

	block, err := aes.NewCipher(keys.encKey)
	if err != nil {
		return nil, err
	}

	overhead := int64(aes256SaltSize + aesPvvSize + aesMacSize)
	if compressedSize < overhead {
		return nil, errors.New("zip: invalid aes file size (too small)")
	}
	// The actual encrypted data size
	dataSize := compressedSize - overhead

	return &aesReader{
		// limitR reads exactly the encrypted payload
		limitR: io.LimitReader(src, dataSize),
		// macSrc is the underlying reader, used to read the MAC after payload
		macSrc:     src,
		stream:     newWinZipCounter(block),
		mac:        hmac.New(sha1.New, keys.macKey),
		checkOnEOF: true,
	}, nil
}

func (r *aesReader) Read(p []byte) (int, error) {
	n, err := r.limitR.Read(p)
	if n > 0 {
		// Update MAC with the bytes read (which are Ciphertext)
		r.mac.Write(p[:n])
		// Decrypt in place
		r.stream.XORKeyStream(p[:n], p[:n])
	}

	// If we hit EOF of the payload, verify the MAC immediately
	if err == io.EOF && r.checkOnEOF {
		expected := make([]byte, aesMacSize)
		// Read the MAC from the remaining bytes in source
		if _, macErr := io.ReadFull(r.macSrc, expected); macErr != nil {
			return n, fmt.Errorf("read auth mac: %w", macErr)
		}

		calculated := r.mac.Sum(nil)[:aesMacSize]
		if !bytes.Equal(calculated, expected) {
			return n, errors.New("zip: aes authentication failed")
		}
		// Verification successful
		return n, io.EOF
	}

	return n, err
}

// aesKeys holds keys derived from the password.
type aesKeys struct {
	encKey []byte // AES encryption key
	macKey []byte // HMAC signing key
	pvv    []byte // Password verification value
}

// deriveAesKeys generates keys using PBKDF2 (RFC 2898)
func deriveAesKeys(password string, salt []byte) aesKeys {
	// WinZip AES-256 uses 1000 iterations of PBKDF2-HMAC-SHA1
	// Need: 32 (AES) + 32 (HMAC) + 2 (PVV) = 66 bytes
	const keyLen = 2*aes256KeySize + aesPvvSize
	dk := pbkdf2([]byte(password), salt, 1000, keyLen, sha1.New)

	return aesKeys{
		encKey: dk[:aes256KeySize],
		macKey: dk[aes256KeySize : 2*aes256KeySize],
		pvv:    dk[2*aes256KeySize : 2*aes256KeySize+aesPvvSize],
	}
}

// winZipCounter implements cipher.Stream for WinZip AES-CTR mode.
// Note: WinZip uses Little Endian increment for the 128-bit counter,
// whereas standard Go cipher.NewCTR uses Big Endian.
type winZipCounter struct {
	block   cipher.Block
	counter [16]byte
	buffer  []byte
	pos     int
}

func newWinZipCounter(block cipher.Block) *winZipCounter {
	c := &winZipCounter{
		block:  block,
		buffer: make([]byte, aes.BlockSize),
	}
	c.counter[0] = 1 // Initial counter value
	return c
}

func (c *winZipCounter) XORKeyStream(dst, src []byte) {
	for i := range src {
		if c.pos == 0 {
			// Encrypt counter to generate keystream block
			c.block.Encrypt(c.buffer[:], c.counter[:])

			// Increment counter (Little Endian 128-bit)
			for j := 0; j < aes.BlockSize; j++ {
				c.counter[j]++
				if c.counter[j] != 0 {
					break // No carry, stop
				}
			}
		}
		dst[i] = src[i] ^ c.buffer[c.pos]
		c.pos = (c.pos + 1) % aes.BlockSize
	}
}

// pbkdf2 implements PBKDF2 with the HMAC variant using the supplied hash function.
func pbkdf2(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)

	for block := 1; block <= numBlocks; block++ {
		// U_1 = PRF(password, salt || INT_32_BE(i))
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dk = prf.Sum(dk)

		T := dk[len(dk)-hashLen:]
		copy(U, T)

		// U_n = PRF(password, U_(n-1))
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = U[:0]
			U = prf.Sum(U)
			for x := range U {
				T[x] ^= U[x]
			}
		}
	}
	return dk[:keyLen]
}
