package gozip

import (
	"encoding/binary"
	"math"
	"path"
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
}

func (h localFileHeader) encode(f *file) []byte {
	filename := path.Join(f.path, f.name)
	if f.isDir {
		filename += "/"
	}
	filenameLen := len(filename)

	// Fixed size (30 bytes) + variable filename length
	// Signature(4) + Header(26) = 30 bytes
	buf := make([]byte, 30+filenameLen)

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

	copy(buf[30:], filename)

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
}

func (d centralDirectory) encode(f *file) []byte {
	filename := path.Join(f.path, f.name)
	if f.isDir {
		filename += "/"
	}

	extraFieldTotalLen := 0
	for _, field := range f.extraField {
		extraFieldTotalLen += len(field.Data)
	}

	// Fixed header (46 bytes) + Filename + Extra Fields + Comment
	// Signature(4) + Header(42) = 46 bytes
	totalSize := 46 + len(filename) + extraFieldTotalLen + len(f.config.Comment)
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
	copy(buf[offset:], filename)
	offset += len(filename)

	// Write Extra Fields
	for _, field := range f.extraField {
		copy(buf[offset:], field.Data)
		offset += len(field.Data)
	}

	// Write Comment
	copy(buf[offset:], f.config.Comment)

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
}

func encodeEndOfCentralDirRecord(z *Zip, centralDirSize uint64, centralDirOffset uint64) []byte {
	commentLen := len(z.comment)
	
	// Fixed header (22 bytes) + Comment length
	// Signature(4) + Header(18) = 22 bytes
	buf := make([]byte, 22+commentLen)

	binary.LittleEndian.PutUint32(buf[0:4], __END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.LittleEndian.PutUint16(buf[4:6], 0) // ThisDiskNum
	binary.LittleEndian.PutUint16(buf[6:8], 0) // DiskNumWithTheStartOfCentralDir
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(z.files))) // TotalNumberOfEntriesOnThisDisk
	binary.LittleEndian.PutUint16(buf[10:12], uint16(len(z.files))) // TotalNumberOfEntries
	binary.LittleEndian.PutUint32(buf[12:16], uint32(min(math.MaxUint32, centralDirSize)))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(min(math.MaxUint32, centralDirOffset)))
	binary.LittleEndian.PutUint16(buf[20:22], uint16(commentLen))

	copy(buf[22:], z.comment)

	return buf
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

func encodeZip64EndOfCentralDirectoryRecord(z *Zip, centralDirSize uint64, centralDirOffset uint64) []byte {
	// Fixed size: Signature(4) + Header(56) = 60 bytes
	buf := make([]byte, 60)

	binary.LittleEndian.PutUint32(buf[0:4], __ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.LittleEndian.PutUint64(buf[4:12], 44) // Size of the rest of the record
	binary.LittleEndian.PutUint16(buf[12:14], 1) // VersionMadeBy
	binary.LittleEndian.PutUint16(buf[14:16], 1) // VersionNeededToExtract
	binary.LittleEndian.PutUint32(buf[16:20], 0) // ThisDiskNum
	binary.LittleEndian.PutUint32(buf[20:24], 0) // DiskNumWithTheStartOfCentralDir
	binary.LittleEndian.PutUint64(buf[24:32], uint64(len(z.files))) // TotalEntriesDisk
	binary.LittleEndian.PutUint64(buf[32:40], uint64(len(z.files))) // TotalEntries
	binary.LittleEndian.PutUint64(buf[40:48], centralDirSize)
	binary.LittleEndian.PutUint64(buf[48:56], centralDirOffset)

	return buf
}

type zip64EndOfCentralDirectoryLocator struct {
	EndOfCentralDirStartDiskNum uint32
	EndOfCentralDirOffset       uint64
	TotalNumberOfDisks          uint32
}

func encodeZip64EndOfCentralDirectoryLocator(endOfCentralDirOffset uint64) []byte {
	// Fixed size: Signature(4) + Header(16) = 20 bytes
	buf := make([]byte, 20)

	binary.LittleEndian.PutUint32(buf[0:4], __ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE)
	binary.LittleEndian.PutUint32(buf[4:8], 0) // EndOfCentralDirStartDiskNum
	binary.LittleEndian.PutUint64(buf[8:16], endOfCentralDirOffset)
	binary.LittleEndian.PutUint32(buf[16:20], 1) // TotalNumberOfDisks

	return buf
}