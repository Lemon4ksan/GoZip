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

func NewZipCryptoReader(src io.Reader, password string, fileCRC uint32) (io.Reader, error) {
	cipher := newZipCipher(password)

	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, fmt.Errorf("read crypto header: %w", err)
	}
	cipher.Decrypt(header)

	if header[11] != byte(fileCRC >> 24) {
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

type aesWriter struct {
	dest   io.Writer
	stream *winZipCounter
	mac    hash.Hash
}

// NewAes256Writer creates a writer that encrypts data using WinZip AES-256
func NewAes256Writer(dest io.Writer, password string) (io.WriteCloser, error) {
	salt := make([]byte, aes256SaltSize)
	rand.Read(salt)
	keys := deriveAesKeys(password, salt)

	if _, err := dest.Write(salt); err != nil {
		return nil, fmt.Errorf("write salt: %w", err)
	}

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
	buf := make([]byte, len(p))
	w.stream.XORKeyStream(buf, p)

	// Update HMAC with ENCRYPTED data (Encrypt-then-MAC)
	w.mac.Write(buf)

	return w.dest.Write(buf)
}

func (w *aesWriter) Close() error {
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
	src        io.Reader
	stream     *winZipCounter
	mac        hash.Hash

	// HMAC verification state
	macBuf     []byte // Buffer to hold the trailing MAC bytes
	eofReached bool
}

// NewAes256Reader wraps src which MUST contain [Salt][PVV][EncryptedData][MAC]
func NewAes256Reader(src io.Reader, password string) (io.Reader, error) {
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

	return &aesReader{
		src:    src,
		stream: newWinZipCounter(block),
		mac:    hmac.New(sha1.New, keys.macKey),
		macBuf: make([]byte, 0, aesMacSize),
	}, nil
}

func (r *aesReader) Read(p []byte) (int, error) {
	if r.eofReached {
		return 0, io.EOF
	}

	n, err := r.src.Read(p)
	if n > 0 {
		r.mac.Write(p[:n])
		r.stream.XORKeyStream(p[:n], p[:n])
	}
	return n, err
}

// VerifyMAC checks the authentication code.
// It MUST be called after all data is read.
func (r *aesReader) CheckMAC(expectedMac []byte) error {
	calculated := r.mac.Sum(nil)[:aesMacSize]
	if !bytes.Equal(calculated, expectedMac) {
		return fmt.Errorf("hmac mismatch: corrupted data")
	}
	return nil
}

const (
	aes256KeySize  = 32 // 32 bytes = 256 bits
	aes256SaltSize = 16 // 16 bytes for AES-256
	aesMacSize     = 10 // HMAC-SHA1 truncated to 10 bytes
	aesPvvSize     = 2  // Password Verification Value
)

// aesKeys holds derived keys
type aesKeys struct {
	encKey []byte // AES encryption key
	macKey []byte // HMAC signing key
	pvv    []byte // Password verification value
}

// deriveAesKeys generates keys using PBKDF2
func deriveAesKeys(password string, salt []byte) aesKeys {
	// WinZip AES-256 needs:
	// - 32 bytes for AES Key
	// - 32 bytes for HMAC Key
	// - 2 bytes for Password Verification Value
	totalLen := 2*aes256KeySize + aesPvvSize
	dk := key([]byte(password), salt, 1000, totalLen, sha1.New)
	return aesKeys{
		encKey: dk[:aes256KeySize],
		macKey: dk[aes256KeySize : 2*aes256KeySize],
		pvv:    dk[2*aes256KeySize:],
	}
}

// winZipCounter implements cipher.Stream for WinZip AES-CTR mode.
// Standard Go cipher.NewCTR uses BigEndian increment, but WinZip requires LittleEndian.
type winZipCounter struct {
	block   cipher.Block
	counter [16]byte
	buffer  []byte
	pos     int
}

func newWinZipCounter(block cipher.Block) *winZipCounter {
	// Counter starts at 1
	c := &winZipCounter{
		block: block,
		buffer: make([]byte, aes.BlockSize),
	}
	c.counter[0] = 1
	return c
}

func (c *winZipCounter) XORKeyStream(dst, src []byte) {
	for i := range src {
		if c.pos == 0 {
			// Encrypt counter to generate keystream
			c.block.Encrypt(c.buffer[:], c.counter[:])
			// Increment counter (Little Endian 128-bit)
			for j := range aes256SaltSize {
				c.counter[j]++
				if c.counter[j] != 0 {
					break
				}
			}
		}
		dst[i] = src[i] ^ c.buffer[c.pos]
		c.pos = (c.pos + 1) % aes.BlockSize
	}
}

// key derives a key from the password, salt and iteration count, returning a
// []byte of length keylen that can be used as cryptographic key. The key is
// derived based on the method described as PBKDF2 with the HMAC variant using
// the supplied hash function.
func key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		// N.B.: || means concatenation, ^ means XOR
		// for each block T_i = U_1 ^ U_2 ^ ... ^ U_iter
		// U_1 = PRF(password, salt || uint(i))
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
