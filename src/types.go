// Copyright 2025 Lemon4ksan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gozip

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
	__CENTRAL_DIRECTORY_SIGNATURE                      uint32 = 0x02014b50
	__LOCAL_FILE_HEADER_SIGNATURE                      uint32 = 0x04034b50
	__DIGITAL_HEADER_SIGNATURE                         uint32 = 0x05054b50
	__END_OF_CENTRAL_DIRECTORY_SIGNATURE               uint32 = 0x06054b50
	__ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE         uint32 = 0x06064b50
	__ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE uint32 = 0x07064b50
	__ARCHIVE_EXTRA_DATA_SIGNATURE                     uint32 = 0x08064b50
)

type localFileHeader struct {
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

func (h localFileHeader) encode() []byte {
	// Fixed size (30 bytes) + variable filename length
	// Signature(4) + Header(26) = 30 bytes
	buf := make([]byte, 30+h.FilenameLength)
	binary.LittleEndian.PutUint32(buf[0:4], __LOCAL_FILE_HEADER_SIGNATURE)
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

type centralDirectory struct {
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

func readCentralDirEntry(src io.Reader) (centralDirectory, error) {
	buf := make([]byte, 42)
	if _, err := io.ReadFull(src, buf); err != nil {
		return centralDirectory{}, fmt.Errorf("read source: %w", err)
	}

	entry := centralDirectory{
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
			return centralDirectory{}, fmt.Errorf("read source: %w", err)
		}
		entry.Filename = string(filename)
	}
	if entry.ExtraFieldLength > 0 {
		extraField := make([]byte, entry.ExtraFieldLength)
		if _, err := io.ReadFull(src, extraField); err != nil {
			return centralDirectory{}, fmt.Errorf("read source: %w", err)
		}
		entry.ExtraField = parseExtraField(extraField)
	}
	if entry.FileCommentLength > 0 {
		comment := make([]byte, entry.FileCommentLength)
		if _, err := io.ReadFull(src, comment); err != nil {
			return centralDirectory{}, fmt.Errorf("read source: %w", err)
		}
		entry.Comment = string(comment)
	}
	return entry, nil
}

func (d centralDirectory) encode() []byte {
	// Fixed header (46 bytes) + Filename + Extra Fields + Comment
	// Signature(4) + Header(42) = 46 bytes
	totalSize := 46 + d.FilenameLength + d.ExtraFieldLength + d.FileCommentLength
	buf := make([]byte, totalSize)

	binary.LittleEndian.PutUint32(buf[0:4], __CENTRAL_DIRECTORY_SIGNATURE)
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

	// Write Filename
	offset += copy(buf[offset:], d.Filename)

	// Write Extra Fields
	for _, entry := range getSortedExtraField(d.ExtraField) {
		offset += copy(buf[offset:], entry)
	}

	// Write Comment
	copy(buf[offset:], d.Comment)

	return buf
}

type endOfCentralDirectory struct {
	ThisDiskNum                     uint16
	DiskNumWithTheStartOfCentralDir uint16
	TotalNumberOfEntriesOnThisDisk  uint16
	TotalNumberOfEntries            uint16
	CentralDirSize                  uint32
	CentralDirOffset                uint32
	CommentLength                   uint16
	Comment                         string
}

func encodeEndOfCentralDirRecord(entriesNum int, centralDirSize uint64, centralDirOffset uint64, comment string) []byte {
	// Fixed header (22 bytes) + Comment length
	// Signature(4) + Header(18) = 22 bytes
	buf := make([]byte, 22+len(comment))

	binary.LittleEndian.PutUint32(buf[0:4], __END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.LittleEndian.PutUint16(buf[4:6], 0)                                         // ThisDiskNum
	binary.LittleEndian.PutUint16(buf[6:8], 0)                                         // DiskNumWithTheStartOfCentralDir
	binary.LittleEndian.PutUint16(buf[8:10], uint16(min(math.MaxUint16, entriesNum)))  // TotalNumberOfEntriesOnThisDisk
	binary.LittleEndian.PutUint16(buf[10:12], uint16(min(math.MaxUint16, entriesNum))) // TotalNumberOfEntries
	binary.LittleEndian.PutUint32(buf[12:16], uint32(min(math.MaxUint32, centralDirSize)))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(min(math.MaxUint32, centralDirOffset)))
	binary.LittleEndian.PutUint16(buf[20:22], uint16(len(comment)))

	copy(buf[22:], comment)

	return buf
}

func readEndOfCentralDir(src io.Reader) (endOfCentralDirectory, error) {
	buf := make([]byte, 18)
	if _, err := io.ReadFull(src, buf); err != nil {
		return endOfCentralDirectory{}, fmt.Errorf("read source: %w", err)
	}
	end := endOfCentralDirectory{
		ThisDiskNum:                     binary.LittleEndian.Uint16(buf[0:2]),
		DiskNumWithTheStartOfCentralDir: binary.LittleEndian.Uint16(buf[2:4]),
		TotalNumberOfEntriesOnThisDisk:  binary.LittleEndian.Uint16(buf[4:6]),
		TotalNumberOfEntries:            binary.LittleEndian.Uint16(buf[6:8]),
		CentralDirSize:                  binary.LittleEndian.Uint32(buf[8:12]),
		CentralDirOffset:                binary.LittleEndian.Uint32(buf[12:16]),
		CommentLength:                   binary.LittleEndian.Uint16(buf[16:18]),
	}
	if end.CommentLength > 0 {
		buf := make([]byte, end.CommentLength)
		io.ReadFull(src, buf)
		end.Comment = string(buf)
	}

	return end, nil
}

type zip64EndOfCentralDirectory struct {
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

func readZip64EndOfCentralDir(src io.Reader) (zip64EndOfCentralDirectory, error) {
	buf := make([]byte, 52)
	if _, err := io.ReadFull(src, buf); err != nil {
		return zip64EndOfCentralDirectory{}, fmt.Errorf("read source: %w", err)
	}
	return zip64EndOfCentralDirectory{
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

func encodeZip64EndOfCentralDirRecord(entriesNum uint64, centralDirSize uint64, centralDirOffset uint64) []byte {
	// Fixed size: Signature(4) + Header(52) = 56 bytes
	buf := make([]byte, 56)

	binary.LittleEndian.PutUint32(buf[0:4], __ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.LittleEndian.PutUint64(buf[4:12], 44)          // Size of the rest of the record
	binary.LittleEndian.PutUint16(buf[12:14], 45)         // VersionMadeBy
	binary.LittleEndian.PutUint16(buf[14:16], 45)         // VersionNeededToExtract
	binary.LittleEndian.PutUint32(buf[16:20], 0)          // ThisDiskNum
	binary.LittleEndian.PutUint32(buf[20:24], 0)          // DiskNumWithTheStartOfCentralDir
	binary.LittleEndian.PutUint64(buf[24:32], entriesNum) // TotalEntriesDisk
	binary.LittleEndian.PutUint64(buf[32:40], entriesNum) // TotalEntries
	binary.LittleEndian.PutUint64(buf[40:48], centralDirSize)
	binary.LittleEndian.PutUint64(buf[48:56], centralDirOffset)

	return buf
}

type zip64EndOfCentralDirectoryLocator struct {
	EndOfCentralDirStartDiskNum uint32
	Zip64EndOfCentralDirOffset  uint64
	TotalNumberOfDisks          uint32
}

func readZip64EndOfCentralDirLocator(src io.Reader) (zip64EndOfCentralDirectoryLocator, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(src, buf); err != nil {
		return zip64EndOfCentralDirectoryLocator{}, fmt.Errorf("read source: %w", err)
	}
	return zip64EndOfCentralDirectoryLocator{
		EndOfCentralDirStartDiskNum: binary.LittleEndian.Uint32(buf[0:4]),
		Zip64EndOfCentralDirOffset:  binary.LittleEndian.Uint64(buf[4:12]),
		TotalNumberOfDisks:          binary.LittleEndian.Uint32(buf[12:16]),
	}, nil
}

func encodeZip64EndOfCentralDirLocator(endOfCentralDirOffset uint64) []byte {
	// Fixed size: Signature(4) + Header(16) = 20 bytes
	buf := make([]byte, 20)

	binary.LittleEndian.PutUint32(buf[0:4], __ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE)
	binary.LittleEndian.PutUint32(buf[4:8], 0) // EndOfCentralDirStartDiskNum
	binary.LittleEndian.PutUint64(buf[8:16], endOfCentralDirOffset)
	binary.LittleEndian.PutUint32(buf[16:20], 1) // TotalNumberOfDisks

	return buf
}

// getSortedExtraField returns a sorted slice of extra fields for deterministic writes.
func getSortedExtraField(extraField map[uint16][]byte) [][]byte {
	keys := make([]uint16, 0, len(extraField))
	for key, _ := range extraField {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	fields := make([][]byte, len(extraField))
	for i, key := range keys {
		fields[i] = extraField[key]
	}
	return fields
}
