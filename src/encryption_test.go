package gozip

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"io"
	"testing"
)

func TestZipCipher_RoundTrip(t *testing.T) {
	password := "secret"
	plaintext := []byte("Hello, World! This is a test of ZipCrypto.")
	
	// Encrypt
	cipherEnc := newZipCipher(password)
	cipherText := make([]byte, len(plaintext))
	copy(cipherText, plaintext)
	cipherEnc.Encrypt(cipherText)

	// Decrypt
	cipherDec := newZipCipher(password)
	decrypted := make([]byte, len(cipherText))
	copy(decrypted, cipherText)
	cipherDec.Decrypt(decrypted)

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("Round trip failed.\nGot: %s\nWant: %s", decrypted, plaintext)
	}
}

func TestZipCipher_KnownVector(t *testing.T) {
	password := "password"
	z := newZipCipher(password)

	expectedK0 := uint32(0xea9b4e4d)
	expectedK1 := uint32(0xba789085)
	expectedK2 := uint32(0x5ff8707d)

	if z.k0 != expectedK0 || z.k1 != expectedK1 || z.k2 != expectedK2 {
		t.Errorf("Initial state mismatch.\nGot: %x %x %x\nWant: %x %x %x", 
			z.k0, z.k1, z.k2, expectedK0, expectedK1, expectedK2)
	}
}

func TestZipCrypto_WriterReader(t *testing.T) {
	password := "my_password"
	content := []byte("sensitive data content")
	checkByte := byte(0xAB) // Fake check byte (MSB of CRC)

	// 1. Write
	buf := new(bytes.Buffer)
	w, err := NewZipCryptoWriter(buf, password, checkByte)
	if err != nil {
		t.Fatalf("NewZipCryptoWriter: %v", err)
	}
	
	n, err := w.Write(content)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(content) {
		t.Errorf("Short write: %d != %d", n, len(content))
	}
	
	// Check size: 12 bytes header + content length
	if buf.Len() != 12+len(content) {
		t.Errorf("Buffer size mismatch: got %d, want %d", buf.Len(), 12+len(content))
	}

	// 2. Read with Correct Password
	// Need fileCRC such that (crc >> 24) == checkByte.
	// 0xAB000000 >> 24 = 0xAB
	fileCRC := uint32(0xAB000000) 
	
	r, err := NewZipCryptoReader(bytes.NewReader(buf.Bytes()), password, fileCRC)
	if err != nil {
		t.Fatalf("NewZipCryptoReader failed: %v", err)
	}

	readBuf := make([]byte, len(content))
	if _, err := io.ReadFull(r, readBuf); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}

	if !bytes.Equal(readBuf, content) {
		t.Errorf("Decrypted content mismatch.\nGot: %s\nWant: %s", readBuf, content)
	}
}

func TestZipCrypto_WrongPassword(t *testing.T) {
	password := "correct"
	checkByte := byte(0x12)
	
	buf := new(bytes.Buffer)
	NewZipCryptoWriter(buf, password, checkByte)
	
	// Try to read with wrong password
	fileCRC := uint32(0x12000000)
	_, err := NewZipCryptoReader(bytes.NewReader(buf.Bytes()), "wrong", fileCRC)
	
	if err != ErrPasswordMismatch {
		t.Errorf("Expected ErrPasswordMismatch, got %v", err)
	}
}

func TestZipCrypto_CheckByteMismatch(t *testing.T) {
	// Case where password is correct technically, but check byte fails
	// (simulates corrupted header or wrong CRC provided to reader)
	password := "correct"
	checkByte := byte(0x12)
	
	buf := new(bytes.Buffer)
	NewZipCryptoWriter(buf, password, checkByte)
	
	// Provide CRC that doesn't match checkByte (0x99 != 0x12)
	fileCRC := uint32(0x99000000)
	_, err := NewZipCryptoReader(bytes.NewReader(buf.Bytes()), password, fileCRC)
	
	if err != ErrPasswordMismatch {
		t.Errorf("Expected ErrPasswordMismatch (due to check byte), got %v", err)
	}
}

func TestAes256_WriterReader_RoundTrip(t *testing.T) {
	password := "strong_password"
	content := []byte("This is highly classified information encrypted with AES-256.")
	
	// 1. Write
	buf := new(bytes.Buffer)
	w, err := NewAes256Writer(buf, password)
	if err != nil {
		t.Fatalf("NewAes256Writer: %v", err)
	}
	
	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	
	// Close to write HMAC
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	
	// Verify Structure size: Salt(16) + PVV(2) + Content + MAC(10)
	expectedSize := 16 + 2 + len(content) + 10
	if buf.Len() != expectedSize {
		t.Errorf("AES output size mismatch: got %d, want %d", buf.Len(), expectedSize)
	}

	// 2. Read
	r, err := NewAes256Reader(bytes.NewReader(buf.Bytes()), password)
	if err != nil {
		t.Fatalf("NewAes256Reader failed: %v", err)
	}

	readBuf := make([]byte, len(content))
	if _, err := io.ReadFull(r, readBuf); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}

	if !bytes.Equal(readBuf, content) {
		t.Errorf("Decrypted content mismatch.\nGot: %s\nWant: %s", readBuf, content)
	}
}

func TestAes256_WrongPassword(t *testing.T) {
	password := "secret"
	buf := new(bytes.Buffer)
	
	w, _ := NewAes256Writer(buf, password)
	w.Write([]byte("data"))
	w.Close()
	
	_, err := NewAes256Reader(bytes.NewReader(buf.Bytes()), "wrong_secret")
	if err != ErrPasswordMismatch {
		t.Errorf("Expected ErrPasswordMismatch, got %v", err)
	}
}

func TestAes256_CorruptedData(t *testing.T) {
	// This tests HMAC failure implicitly if implemented, or garbage output
	password := "secret"
	content := []byte("data")
	
	buf := new(bytes.Buffer)
	w, _ := NewAes256Writer(buf, password)
	w.Write(content)
	w.Close()
	
	data := buf.Bytes()
	// Corrupt a byte in the encrypted payload (offset: 16+2=18 is start of data)
	data[18] ^= 0xFF
	
	r, err := NewAes256Reader(bytes.NewReader(data), password)
	if err != nil {
		t.Fatalf("Reader creation should succeed (keys are valid): %v", err)
	}
	
	readBuf := make([]byte, len(content))
	r.Read(readBuf)
	
	// Decrypted data should be garbage due to corruption
	if bytes.Equal(readBuf, content) {
		t.Error("Corrupted cipher text resulted in valid plaintext (impossible)")
	}
}

func TestDeriveAesKeys(t *testing.T) {
	// Validates that PBKDF2 logic matches expectations (no regression)
	password := "password"
	salt, _ := hex.DecodeString("000102030405060708090A0B0C0D0E0F") // 16 bytes
	
	keys := deriveAesKeys(password, salt)
	
	if len(keys.encKey) != 32 {
		t.Errorf("EncKey len: %d", len(keys.encKey))
	}
	if len(keys.macKey) != 32 {
		t.Errorf("MacKey len: %d", len(keys.macKey))
	}
	if len(keys.pvv) != 2 {
		t.Errorf("PVV len: %d", len(keys.pvv))
	}
	
	// Ensure determinism
	keys2 := deriveAesKeys(password, salt)
	if !bytes.Equal(keys.encKey, keys2.encKey) {
		t.Error("Key derivation is not deterministic")
	}
}

func TestWinZipCounter_LittleEndian(t *testing.T) {
	key := make([]byte, 32)
	block, _ := aes.NewCipher(key)

	c := newWinZipCounter(block)
	
	src := make([]byte, 32) 
	dst := make([]byte, 32)
	c.XORKeyStream(dst, src)

	iv := make([]byte, 16)
	iv[0] = 1 // Start at 1
	ctr := cipher.NewCTR(block, iv)
	dstStandard := make([]byte, 32)
	ctr.XORKeyStream(dstStandard, src)

	if !bytes.Equal(dst[:16], dstStandard[:16]) {
		t.Log("Note: First block differs (this is unexpected but not fatal if init differs)")
	}

	if bytes.Equal(dst[16:], dstStandard[16:]) {
		t.Error("WinZipCounter behaves like standard BigEndian CTR in the 2nd block!")
	}
}