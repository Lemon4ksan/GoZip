// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
)

// Each record type must be identified using a header signature that identifies the record type.
// Signature values begin with the two byte constant marker of 0x4b50, representing the characters "PK".
const (
	CentralDirectorySignature            uint32 = 0x02014b50
	LocalFileHeaderSignature             uint32 = 0x04034b50
	DigitalHeaderSignature               uint32 = 0x05054b50
	EndOfCentralDirSignature             uint32 = 0x06054b50
	Zip64EndOfCentralDirSignature        uint32 = 0x06064b50
	Zip64EndOfCentralDirLocatorSignature uint32 = 0x07064b50
	ArchiveExtraDataSignature            uint32 = 0x08064b50
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

func (h LocalFileHeader) Encode() []byte {
	// Fixed size (30 bytes) + variable filename length
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
	ExtraField             map[uint16][]byte
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
		extraField := make([]byte, entry.ExtraFieldLength)
		if _, err := io.ReadFull(src, extraField); err != nil {
			return CentralDirectory{}, fmt.Errorf("read extra field: %w", err)
		}
		entry.ExtraField = parseExtraField(extraField)
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

	// Determine deterministic order for extra fields
	sortedFields := getSortedExtraField(d.ExtraField)
	for _, entry := range sortedFields {
		offset += copy(buf[offset:], entry)
	}

	copy(buf[offset:], d.Comment)

	return buf
}

type EndOfCentralDirectory struct {
	ThisDiskNum                     uint16
	DiskNumWithTheStartOfCentralDir uint16
	TotalNumberOfEntriesOnThisDisk  uint16
	TotalNumberOfEntries            uint16
	CentralDirSize                  uint32
	CentralDirOffset                uint32
	CommentLength                   uint16
	Comment                         string
}

func EncodeEndOfCentralDirRecord(entriesNum int, centralDirSize uint64, centralDirOffset uint64, comment string) []byte {
	commentLen := min(len(comment), math.MaxUint16)
	buf := make([]byte, 22+commentLen)

	binary.LittleEndian.PutUint32(buf[0:4], EndOfCentralDirSignature)
	binary.LittleEndian.PutUint16(buf[4:6], 0)
	binary.LittleEndian.PutUint16(buf[6:8], 0)
	binary.LittleEndian.PutUint16(buf[8:10], uint16(min(math.MaxUint16, entriesNum)))
	binary.LittleEndian.PutUint16(buf[10:12], uint16(min(math.MaxUint16, entriesNum)))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(min(math.MaxUint32, centralDirSize)))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(min(math.MaxUint32, centralDirOffset)))
	binary.LittleEndian.PutUint16(buf[20:22], uint16(commentLen))

	copy(buf[22:], comment[:commentLen])

	return buf
}

func ReadEndOfCentralDir(src io.Reader) (EndOfCentralDirectory, error) {
	var buf [18]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return EndOfCentralDirectory{}, fmt.Errorf("read source: %w", err)
	}
	end := EndOfCentralDirectory{
		ThisDiskNum:                     binary.LittleEndian.Uint16(buf[0:2]),
		DiskNumWithTheStartOfCentralDir: binary.LittleEndian.Uint16(buf[2:4]),
		TotalNumberOfEntriesOnThisDisk:  binary.LittleEndian.Uint16(buf[4:6]),
		TotalNumberOfEntries:            binary.LittleEndian.Uint16(buf[6:8]),
		CentralDirSize:                  binary.LittleEndian.Uint32(buf[8:12]),
		CentralDirOffset:                binary.LittleEndian.Uint32(buf[12:16]),
		CommentLength:                   binary.LittleEndian.Uint16(buf[16:18]),
	}
	if end.CommentLength > 0 {
		commentBuf := make([]byte, end.CommentLength)
		if _, err := io.ReadFull(src, commentBuf); err != nil {
			return EndOfCentralDirectory{}, fmt.Errorf("read comment: %w", err)
		}
		end.Comment = string(commentBuf)
	}

	return end, nil
}

type Zip64EndOfCentralDirectory struct {
	Size                            uint64
	VersionMadeBy                   uint16
	VersionNeededToExtract          uint16
	ThisDiskNum                     uint32
	DiskNumWithTheStartOfCentralDir uint32
	TotalNumberOfEntriesOnThisDisk  uint64
	TotalNumberOfEntries            uint64
	CentralDirSize                  uint64
	CentralDirOffset                uint64
}

func ReadZip64EndOfCentralDir(src io.Reader) (Zip64EndOfCentralDirectory, error) {
	var buf [52]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return Zip64EndOfCentralDirectory{}, fmt.Errorf("read source: %w", err)
	}
	return Zip64EndOfCentralDirectory{
		Size:                            binary.LittleEndian.Uint64(buf[0:8]),
		VersionMadeBy:                   binary.LittleEndian.Uint16(buf[8:10]),
		VersionNeededToExtract:          binary.LittleEndian.Uint16(buf[10:12]),
		ThisDiskNum:                     binary.LittleEndian.Uint32(buf[12:16]),
		DiskNumWithTheStartOfCentralDir: binary.LittleEndian.Uint32(buf[16:20]),
		TotalNumberOfEntriesOnThisDisk:  binary.LittleEndian.Uint64(buf[20:28]),
		TotalNumberOfEntries:            binary.LittleEndian.Uint64(buf[28:36]),
		CentralDirSize:                  binary.LittleEndian.Uint64(buf[36:44]),
		CentralDirOffset:                binary.LittleEndian.Uint64(buf[44:52]),
	}, nil
}

func EncodeZip64EndOfCentralDirRecord(entriesNum uint64, centralDirSize uint64, centralDirOffset uint64) []byte {
	buf := make([]byte, 56)

	binary.LittleEndian.PutUint32(buf[0:4], Zip64EndOfCentralDirSignature)
	binary.LittleEndian.PutUint64(buf[4:12], 44)
	binary.LittleEndian.PutUint16(buf[12:14], 45)
	binary.LittleEndian.PutUint16(buf[14:16], 45)
	binary.LittleEndian.PutUint32(buf[16:20], 0)
	binary.LittleEndian.PutUint32(buf[20:24], 0)
	binary.LittleEndian.PutUint64(buf[24:32], entriesNum)
	binary.LittleEndian.PutUint64(buf[32:40], entriesNum)
	binary.LittleEndian.PutUint64(buf[40:48], centralDirSize)
	binary.LittleEndian.PutUint64(buf[48:56], centralDirOffset)

	return buf
}

type Zip64EndOfCentralDirectoryLocator struct {
	EndOfCentralDirStartDiskNum uint32
	Zip64EndOfCentralDirOffset  uint64
	TotalNumberOfDisks          uint32
}

func ReadZip64EndOfCentralDirLocator(src io.Reader) (Zip64EndOfCentralDirectoryLocator, error) {
	var buf [16]byte
	if _, err := io.ReadFull(src, buf[:]); err != nil {
		return Zip64EndOfCentralDirectoryLocator{}, fmt.Errorf("read source: %w", err)
	}
	return Zip64EndOfCentralDirectoryLocator{
		EndOfCentralDirStartDiskNum: binary.LittleEndian.Uint32(buf[0:4]),
		Zip64EndOfCentralDirOffset:  binary.LittleEndian.Uint64(buf[4:12]),
		TotalNumberOfDisks:          binary.LittleEndian.Uint32(buf[12:16]),
	}, nil
}

func EncodeZip64EndOfCentralDirLocator(endOfCentralDirOffset uint64) []byte {
	buf := make([]byte, 20)

	binary.LittleEndian.PutUint32(buf[0:4], Zip64EndOfCentralDirLocatorSignature)
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	binary.LittleEndian.PutUint64(buf[8:16], endOfCentralDirOffset)
	binary.LittleEndian.PutUint32(buf[16:20], 1)

	return buf
}

// getSortedExtraField returns a sorted slice of extra fields for deterministic writes.
func getSortedExtraField(extraField map[uint16][]byte) [][]byte {
	if len(extraField) == 0 {
		return nil
	}
	keys := make([]uint16, 0, len(extraField))
	for key := range extraField {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	fields := make([][]byte, len(extraField))
	for i, key := range keys {
		fields[i] = extraField[key]
	}
	return fields
}

// parseExtraField converts raw extra field bytes into a map keyed by tag IDs.
func parseExtraField(extraField []byte) map[uint16][]byte {
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
