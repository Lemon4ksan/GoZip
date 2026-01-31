// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/lemon4ksan/gozip/internal/sys"
)

// Each record type must be identified using a header signature that identifies the record type.
// Signature values begin with the two byte constant marker of 0x4b50, representing the characters "PK".
const (
	CentralDirectorySignature uint32 = 0x02014b50
	LocalFileHeaderSignature  uint32 = 0x04034b50
	DigitalHeaderSignature    uint32 = 0x05054b50
	EOCDSignature             uint32 = 0x06054b50
	Zip64EOCDSignature        uint32 = 0x06064b50
	Zip64EOCDLocatorSignature uint32 = 0x07064b50
	ArchiveExtraDataSignature uint32 = 0x08064b50
	DataDescriptorSignature   uint32 = 0x08074b50
)

const (
	MaxUint16 = 1<<16 - 1
	MaxUint32 = 1<<32 - 1
)

const (
	Zip64ExtraFieldTag uint16 = 0x0001
	NTFSFieldTag       uint16 = 0x000A
	AESEncryptionTag   uint16 = 0x9901
)

type LocalFileHeader struct {
	VersionNeededToExtract uint16
	GeneralPurposeBitFlag  uint16
	CompressionMethod      uint16
	LastModFileTime        uint16
	LastModFileDate        uint16
	CRC32                  uint32
	CompressedSize         uint32
	UncompressedSize       uint32
	FilenameLength         uint16
	ExtraFieldLength       uint16
	Filename               string
	ExtraField             []byte
}

func ReadLocalFileHeader(src io.Reader) (LocalFileHeader, error) {
	var buf [26]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return LocalFileHeader{}, fmt.Errorf("read source: %w", err)
	}

	entry := LocalFileHeader{
		VersionNeededToExtract: binary.LittleEndian.Uint16(buf[0:2]),
		GeneralPurposeBitFlag:  binary.LittleEndian.Uint16(buf[2:4]),
		CompressionMethod:      binary.LittleEndian.Uint16(buf[4:6]),
		LastModFileTime:        binary.LittleEndian.Uint16(buf[6:8]),
		LastModFileDate:        binary.LittleEndian.Uint16(buf[8:10]),
		CRC32:                  binary.LittleEndian.Uint32(buf[10:14]),
		CompressedSize:         binary.LittleEndian.Uint32(buf[14:18]),
		UncompressedSize:       binary.LittleEndian.Uint32(buf[18:22]),
		FilenameLength:         binary.LittleEndian.Uint16(buf[22:24]),
		ExtraFieldLength:       binary.LittleEndian.Uint16(buf[24:26]),
	}

	if entry.FilenameLength > 0 {
		filename := make([]byte, entry.FilenameLength)
		if _, err := io.ReadFull(src, filename); err != nil {
			return LocalFileHeader{}, fmt.Errorf("read filename: %w", err)
		}
		entry.Filename = string(filename)
	}

	if entry.ExtraFieldLength > 0 {
		entry.ExtraField = make([]byte, entry.ExtraFieldLength)
		if _, err := io.ReadFull(src, entry.ExtraField); err != nil {
			return LocalFileHeader{}, fmt.Errorf("read extra field: %w", err)
		}
	}

	return entry, nil
}

func (h LocalFileHeader) Encode() []byte {
	size := 30 + h.FilenameLength + h.ExtraFieldLength
	buf := make([]byte, size)

	binary.LittleEndian.PutUint32(buf[0:4], LocalFileHeaderSignature)
	binary.LittleEndian.PutUint16(buf[4:6], h.VersionNeededToExtract)
	binary.LittleEndian.PutUint16(buf[6:8], h.GeneralPurposeBitFlag)
	binary.LittleEndian.PutUint16(buf[8:10], h.CompressionMethod)
	binary.LittleEndian.PutUint16(buf[10:12], h.LastModFileTime)
	binary.LittleEndian.PutUint16(buf[12:14], h.LastModFileDate)
	binary.LittleEndian.PutUint32(buf[14:18], h.CRC32)
	binary.LittleEndian.PutUint32(buf[18:22], h.CompressedSize)
	binary.LittleEndian.PutUint32(buf[22:26], h.UncompressedSize)
	binary.LittleEndian.PutUint16(buf[26:28], h.FilenameLength)
	binary.LittleEndian.PutUint16(buf[28:30], h.ExtraFieldLength)

	copy(buf[30:], h.Filename)
	copy(buf[30+h.FilenameLength:], h.ExtraField)

	return buf
}

type CentralDirectory struct {
	VersionMadeBy          uint16
	VersionNeededToExtract uint16
	GeneralPurposeBitFlag  uint16
	CompressionMethod      uint16
	LastModFileTime        uint16
	LastModFileDate        uint16
	CRC32                  uint32
	CompressedSize         uint32
	UncompressedSize       uint32
	FilenameLength         uint16
	ExtraFieldLength       uint16
	FileCommentLength      uint16
	DiskNumberStart        uint16
	InternalFileAttributes uint16
	ExternalFileAttributes uint32
	LocalHeaderOffset      uint32
	Filename               string
	ExtraField             []byte
	Comment                string
}

func ReadCentralDirEntry(src io.Reader) (CentralDirectory, error) {
	var buf [42]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return CentralDirectory{}, fmt.Errorf("read source: %w", err)
	}

	entry := CentralDirectory{
		VersionMadeBy:          binary.LittleEndian.Uint16(buf[0:2]),
		VersionNeededToExtract: binary.LittleEndian.Uint16(buf[2:4]),
		GeneralPurposeBitFlag:  binary.LittleEndian.Uint16(buf[4:6]),
		CompressionMethod:      binary.LittleEndian.Uint16(buf[6:8]),
		LastModFileTime:        binary.LittleEndian.Uint16(buf[8:10]),
		LastModFileDate:        binary.LittleEndian.Uint16(buf[10:12]),
		CRC32:                  binary.LittleEndian.Uint32(buf[12:16]),
		CompressedSize:         binary.LittleEndian.Uint32(buf[16:20]),
		UncompressedSize:       binary.LittleEndian.Uint32(buf[20:24]),
		FilenameLength:         binary.LittleEndian.Uint16(buf[24:26]),
		ExtraFieldLength:       binary.LittleEndian.Uint16(buf[26:28]),
		FileCommentLength:      binary.LittleEndian.Uint16(buf[28:30]),
		DiskNumberStart:        binary.LittleEndian.Uint16(buf[30:32]),
		InternalFileAttributes: binary.LittleEndian.Uint16(buf[32:34]),
		ExternalFileAttributes: binary.LittleEndian.Uint32(buf[34:38]),
		LocalHeaderOffset:      binary.LittleEndian.Uint32(buf[38:42]),
	}

	if entry.FilenameLength > 0 {
		filename := make([]byte, entry.FilenameLength)
		if _, err := io.ReadFull(src, filename); err != nil {
			return CentralDirectory{}, fmt.Errorf("read filename: %w", err)
		}
		entry.Filename = string(filename)
	}

	if entry.ExtraFieldLength > 0 {
		entry.ExtraField = make([]byte, entry.ExtraFieldLength)
		if _, err := io.ReadFull(src, entry.ExtraField); err != nil {
			return CentralDirectory{}, fmt.Errorf("read extra field: %w", err)
		}
	}

	if entry.FileCommentLength > 0 {
		comment := make([]byte, entry.FileCommentLength)
		if _, err := io.ReadFull(src, comment); err != nil {
			return CentralDirectory{}, fmt.Errorf("read comment: %w", err)
		}
		entry.Comment = string(comment)
	}

	return entry, nil
}

func (d CentralDirectory) Encode() []byte {
	totalSize := 46 + int(d.FilenameLength) + int(d.ExtraFieldLength) + int(d.FileCommentLength)
	buf := make([]byte, totalSize)

	binary.LittleEndian.PutUint32(buf[0:4], CentralDirectorySignature)
	binary.LittleEndian.PutUint16(buf[4:6], d.VersionMadeBy)
	binary.LittleEndian.PutUint16(buf[6:8], d.VersionNeededToExtract)
	binary.LittleEndian.PutUint16(buf[8:10], d.GeneralPurposeBitFlag)
	binary.LittleEndian.PutUint16(buf[10:12], d.CompressionMethod)
	binary.LittleEndian.PutUint16(buf[12:14], d.LastModFileTime)
	binary.LittleEndian.PutUint16(buf[14:16], d.LastModFileDate)
	binary.LittleEndian.PutUint32(buf[16:20], d.CRC32)
	binary.LittleEndian.PutUint32(buf[20:24], d.CompressedSize)
	binary.LittleEndian.PutUint32(buf[24:28], d.UncompressedSize)
	binary.LittleEndian.PutUint16(buf[28:30], d.FilenameLength)
	binary.LittleEndian.PutUint16(buf[30:32], d.ExtraFieldLength)
	binary.LittleEndian.PutUint16(buf[32:34], d.FileCommentLength)
	binary.LittleEndian.PutUint16(buf[34:36], d.DiskNumberStart)
	binary.LittleEndian.PutUint16(buf[36:38], d.InternalFileAttributes)
	binary.LittleEndian.PutUint32(buf[38:42], d.ExternalFileAttributes)
	binary.LittleEndian.PutUint32(buf[42:46], d.LocalHeaderOffset)

	offset := 46

	offset += copy(buf[offset:], d.Filename)
	offset += copy(buf[offset:], d.ExtraField)
	copy(buf[offset:], d.Comment)

	return buf
}

type EOCD struct {
	ThisDiskNum                     uint16
	DiskNumWithTheStartOfCentralDir uint16
	TotalNumberOfEntriesOnThisDisk  uint16
	EntriesNum                      uint16
	CentralDirSize                  uint32
	CentralDirOffset                uint32
	CommentLength                   uint16
	Comment                         string
}

func ReadEOCD(src io.Reader) (EOCD, error) {
	var buf [18]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return EOCD{}, fmt.Errorf("read source: %w", err)
	}
	end := EOCD{
		ThisDiskNum:                     binary.LittleEndian.Uint16(buf[0:2]),
		DiskNumWithTheStartOfCentralDir: binary.LittleEndian.Uint16(buf[2:4]),
		TotalNumberOfEntriesOnThisDisk:  binary.LittleEndian.Uint16(buf[4:6]),
		EntriesNum:                      binary.LittleEndian.Uint16(buf[6:8]),
		CentralDirSize:                  binary.LittleEndian.Uint32(buf[8:12]),
		CentralDirOffset:                binary.LittleEndian.Uint32(buf[12:16]),
		CommentLength:                   binary.LittleEndian.Uint16(buf[16:18]),
	}
	if end.CommentLength > 0 {
		commentBuf := make([]byte, end.CommentLength)
		if _, err := io.ReadFull(src, commentBuf); err != nil {
			return EOCD{}, fmt.Errorf("read comment: %w", err)
		}
		end.Comment = string(commentBuf)
	}

	return end, nil
}

func EncodeEOCD(entriesNum int, centralDirSize int64, centralDirOffset int64, comment string) []byte {
	commentLen := min(len(comment), MaxUint16)
	buf := make([]byte, 22+commentLen)

	binary.LittleEndian.PutUint32(buf[0:4], EOCDSignature)
	binary.LittleEndian.PutUint16(buf[4:6], 0)
	binary.LittleEndian.PutUint16(buf[6:8], 0)
	binary.LittleEndian.PutUint16(buf[8:10], uint16(min(MaxUint16, entriesNum)))
	binary.LittleEndian.PutUint16(buf[10:12], uint16(min(MaxUint16, entriesNum)))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(min(MaxUint32, centralDirSize)))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(min(MaxUint32, centralDirOffset)))
	binary.LittleEndian.PutUint16(buf[20:22], uint16(commentLen))

	copy(buf[22:], comment[:commentLen])

	return buf
}

type Zip64EOCD struct {
	Size                            uint64
	VersionMadeBy                   uint16
	VersionNeededToExtract          uint16
	ThisDiskNum                     uint32
	DiskNumWithTheStartOfCentralDir uint32
	TotalNumberOfEntriesOnThisDisk  uint64
	EntriesNum                      uint64
	CentralDirSize                  uint64
	CentralDirOffset                uint64
}

func ReadZip64EOCD(src io.Reader) (Zip64EOCD, error) {
	var buf [52]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return Zip64EOCD{}, fmt.Errorf("read source: %w", err)
	}
	return Zip64EOCD{
		Size:                            binary.LittleEndian.Uint64(buf[0:8]),
		VersionMadeBy:                   binary.LittleEndian.Uint16(buf[8:10]),
		VersionNeededToExtract:          binary.LittleEndian.Uint16(buf[10:12]),
		ThisDiskNum:                     binary.LittleEndian.Uint32(buf[12:16]),
		DiskNumWithTheStartOfCentralDir: binary.LittleEndian.Uint32(buf[16:20]),
		TotalNumberOfEntriesOnThisDisk:  binary.LittleEndian.Uint64(buf[20:28]),
		EntriesNum:                      binary.LittleEndian.Uint64(buf[28:36]),
		CentralDirSize:                  binary.LittleEndian.Uint64(buf[36:44]),
		CentralDirOffset:                binary.LittleEndian.Uint64(buf[44:52]),
	}, nil
}

func EncodeZip64EOCDRecord(entriesNum int, centralDirSize int64, centralDirOffset int64) []byte {
	var buf [56]byte
	binary.LittleEndian.PutUint32(buf[0:4], Zip64EOCDSignature)
	binary.LittleEndian.PutUint64(buf[4:12], 44)
	binary.LittleEndian.PutUint16(buf[12:14], 45)
	binary.LittleEndian.PutUint16(buf[14:16], 45)
	binary.LittleEndian.PutUint32(buf[16:20], 0)
	binary.LittleEndian.PutUint32(buf[20:24], 0)
	binary.LittleEndian.PutUint64(buf[24:32], uint64(entriesNum))
	binary.LittleEndian.PutUint64(buf[32:40], uint64(entriesNum))
	binary.LittleEndian.PutUint64(buf[40:48], uint64(centralDirSize))
	binary.LittleEndian.PutUint64(buf[48:56], uint64(centralDirOffset))
	return buf[:]
}

type Zip64EOCDLocator struct {
	EndOfCentralDirStartDiskNum uint32
	Zip64EndOfCentralDirOffset  uint64
	TotalNumberOfDisks          uint32
}

func ReadZip64EOCDLocator(src io.Reader) (Zip64EOCDLocator, error) {
	var buf [16]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return Zip64EOCDLocator{}, fmt.Errorf("read source: %w", err)
	}
	return Zip64EOCDLocator{
		EndOfCentralDirStartDiskNum: binary.LittleEndian.Uint32(buf[0:4]),
		Zip64EndOfCentralDirOffset:  binary.LittleEndian.Uint64(buf[4:12]),
		TotalNumberOfDisks:          binary.LittleEndian.Uint32(buf[12:16]),
	}, nil
}

func EncodeZip64EOCDLocator(eocdOffset int64) []byte {
	var buf [20]byte
	binary.LittleEndian.PutUint32(buf[0:4], Zip64EOCDLocatorSignature)
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(eocdOffset))
	binary.LittleEndian.PutUint32(buf[16:20], 1)
	return buf[:]
}

type SharedEntry struct {
	VersionNeededToExtract uint16
	GeneralPurposeBitFlag  uint16
	CompressionMethod      uint16
	LastModFileTime        uint16
	LastModFileDate        uint16
	CRC32                  uint32
	CompressedSize         uint32
	UncompressedSize       uint32
	FilenameLength         uint16
	ExtraFieldLength       uint16
	LocalHeaderOffset      uint32
	Filename               string
	ExtraField             []byte
}

func SharedEntryFromLocal(entry LocalFileHeader) SharedEntry {
	return SharedEntry{
		VersionNeededToExtract: entry.VersionNeededToExtract,
		GeneralPurposeBitFlag:  entry.GeneralPurposeBitFlag,
		CompressionMethod:      entry.CompressionMethod,
		LastModFileTime:        entry.LastModFileTime,
		LastModFileDate:        entry.LastModFileDate,
		CRC32:                  entry.CRC32,
		CompressedSize:         entry.CompressedSize,
		UncompressedSize:       entry.UncompressedSize,
		FilenameLength:         entry.FilenameLength,
		ExtraFieldLength:       entry.ExtraFieldLength,
		Filename:               entry.Filename,
		ExtraField:             entry.ExtraField,
	}
}

func SharedEntryFromCD(entry CentralDirectory) SharedEntry {
	return SharedEntry{
		VersionNeededToExtract: entry.VersionNeededToExtract,
		GeneralPurposeBitFlag:  entry.GeneralPurposeBitFlag,
		CompressionMethod:      entry.CompressionMethod,
		LastModFileTime:        entry.LastModFileTime,
		LastModFileDate:        entry.LastModFileDate,
		CRC32:                  entry.CRC32,
		CompressedSize:         entry.CompressedSize,
		UncompressedSize:       entry.UncompressedSize,
		FilenameLength:         entry.FilenameLength,
		ExtraFieldLength:       entry.ExtraFieldLength,
		LocalHeaderOffset:      entry.LocalHeaderOffset,
		Filename:               entry.Filename,
		ExtraField:             entry.ExtraField,
	}
}

func ParseExtraField(extraField []byte) map[uint16][]byte {
	m := make(map[uint16][]byte)

	for offset := 0; offset < len(extraField); {
		if offset+4 > len(extraField) {
			break
		}

		tag := binary.LittleEndian.Uint16(extraField[offset : offset+2])
		size := int(binary.LittleEndian.Uint16(extraField[offset+2 : offset+4]))

		offset += 4
		if offset+size > len(extraField) {
			break
		}

		m[tag] = extraField[offset-4 : offset+size]
		offset += size
	}
	return m
}

func EncodeZip64ExtraField(uncompSize, compSize, headerOffset int64) []byte {
	data := make([]byte, 4, 28)

	binary.LittleEndian.PutUint16(data[0:2], Zip64ExtraFieldTag)

	if uncompSize > MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(uncompSize))
	}
	if compSize > MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(compSize))
	}
	if headerOffset > MaxUint32 {
		data = binary.LittleEndian.AppendUint64(data, uint64(headerOffset))
	}

	binary.LittleEndian.PutUint16(data[2:4], uint16(len(data)-4))
	return data
}

func EncodeZip64LocalExtraField(uncompSize, compSize int64) []byte {
	var data [20]byte
	binary.LittleEndian.PutUint16(data[0:2], Zip64ExtraFieldTag)
	binary.LittleEndian.PutUint16(data[2:4], 16) // Size of payload
	binary.LittleEndian.PutUint64(data[4:12], uint64(uncompSize))
	binary.LittleEndian.PutUint64(data[12:20], uint64(compSize))
	return data[:]
}

func ParseNTFSExtraField(data []byte) map[string]interface{} {
	return map[string]interface{}{
		"LastWriteTime":  binary.LittleEndian.Uint64(data[12:20]),
		"LastAccessTime": binary.LittleEndian.Uint64(data[12:20]),
		"CreationTime":   binary.LittleEndian.Uint64(data[28:36]),
	}
}

func EncodeNTFSExtraField(metadata map[string]interface{}) []byte {
	var mtime, atime, ctime uint64
	if val, ok := metadata["LastWriteTime"]; ok {
		if t, ok := val.(uint64); ok {
			mtime = t
		}
	}
	if val, ok := metadata["LastAccessTime"]; ok {
		if t, ok := val.(uint64); ok {
			atime = t
		}
	}
	if val, ok := metadata["CreationTime"]; ok {
		if t, ok := val.(uint64); ok {
			ctime = t
		}
	}

	// Tag(2) + Size(2) + Reserved(4) + Attr1(2) + Size1(2) + Mtime(8) + Atime(8) + Ctime(8)
	var data [36]byte
	binary.LittleEndian.PutUint16(data[0:2], NTFSFieldTag)
	binary.LittleEndian.PutUint16(data[2:4], 32)
	binary.LittleEndian.PutUint32(data[4:8], 0)
	binary.LittleEndian.PutUint16(data[8:10], 1)
	binary.LittleEndian.PutUint16(data[10:12], 24)
	binary.LittleEndian.PutUint64(data[12:20], mtime)
	binary.LittleEndian.PutUint64(data[20:28], atime)
	binary.LittleEndian.PutUint64(data[28:36], ctime)
	return data[:]
}

func ParseFileMode(entry CentralDirectory) fs.FileMode {
	var mode fs.FileMode
	hostSystem := sys.HostSystem(entry.VersionMadeBy >> 8)

	if hostSystem.IsUnix() {
		unixMode := uint32(entry.ExternalFileAttributes >> 16)
		mode = fs.FileMode(unixMode & 0777)

		switch unixMode & sys.S_IFMT {
		case sys.S_IFDIR:
			mode |= fs.ModeDir
		case sys.S_IFLNK:
			mode |= fs.ModeSymlink
		case sys.S_IFSOCK:
			mode |= fs.ModeSocket
		case sys.S_IFIFO:
			mode |= fs.ModeNamedPipe
		case sys.S_IFCHR:
			mode |= fs.ModeCharDevice
		case sys.S_IFBLK:
			mode |= fs.ModeDevice
		}
		return mode
	}

	if hostSystem.IsWindows() {
		isDir := strings.HasSuffix(entry.Filename, "/") || (entry.ExternalFileAttributes&0x10 != 0)

		if isDir {
			mode = 0755 | fs.ModeDir
		} else {
			mode = 0644
		}

		if entry.ExternalFileAttributes&0x01 != 0 {
			mode &^= 0222 // Remove write permission (a-w)
		}
		return mode
	}

	if strings.HasSuffix(entry.Filename, "/") {
		return 0755 | fs.ModeDir
	}
	return 0644
}

func EncodeDataDescriptor(crc uint32, compSize, uncompSize int64) []byte {
	if compSize > MaxUint32 || uncompSize > MaxUint32 {
		// ZIP64 Data Descriptor: Sig(4) + CRC(4) + Comp(8) + Uncomp(8)
		var buf [24]byte
		binary.LittleEndian.PutUint32(buf[0:4], DataDescriptorSignature)
		binary.LittleEndian.PutUint32(buf[4:8], crc)
		binary.LittleEndian.PutUint64(buf[8:16], uint64(compSize))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(uncompSize))
		return buf[:]
	}

	// Standard Data Descriptor: Sig(4) + CRC(4) + Comp(4) + Uncomp(4)
	var buf [16]byte
	binary.LittleEndian.PutUint32(buf[0:4], DataDescriptorSignature)
	binary.LittleEndian.PutUint32(buf[4:8], crc)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(compSize))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(uncompSize))
	return buf[:]
}

func EncodeAESExtraField(compMethod uint16) []byte {
	var data [11]byte
	binary.LittleEndian.PutUint16(data[0:2], AESEncryptionTag)
	binary.LittleEndian.PutUint16(data[2:4], 7)
	binary.LittleEndian.PutUint16(data[4:6], 0x0002) // Version 2
	data[6] = 'A'
	data[7] = 'E'
	data[8] = 0x03 // AES-256
	binary.LittleEndian.PutUint16(data[9:11], compMethod)
	return data[:]
}
