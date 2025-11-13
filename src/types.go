package gozip

import (
	"bytes"
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
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __LOCAL_FILE_HEADER_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, h)
	buf.WriteString(path.Join(f.path, f.name))
	if f.isDir {
		buf.WriteString("/")
	}
	return buf.Bytes()
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
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __CENTRAL_DIRECTORY_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, d)
	buf.WriteString(path.Join(f.path, f.name))
	if f.isDir {
		buf.WriteString("/")
	}
	for _, field := range f.extraField {
		buf.Write(field.Data)
	}
	buf.WriteString(f.config.Comment)
	return buf.Bytes()
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

// encodeEndOfCentralDirRecord creates the end of central directory record.
// This marks the end of the ZIP file and contains archive-wide information.
func encodeEndOfCentralDirRecord(z *Zip, centralDirSize uint64, centralDirOffset uint64) []byte {
	record := endOfCentralDirectory{
		ThisDiskNum:                     0,
		DiskNumWithTheStartOfCentralDir: 0,
		TotalNumberOfEntriesOnThisDisk:  uint16(len(z.files)),
		TotalNumberOfEntries:            uint16(len(z.files)),
		CentralDirSize:                  uint32(min(math.MaxUint32, centralDirSize)),
		CentralDirOffset:                uint32(min(math.MaxUint32, centralDirOffset)),
		CommentLength:                   uint16(len(z.comment)),
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, record)
	buf.WriteString(z.comment)
	return buf.Bytes()
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
	record := zip64EndOfCentralDirectory{
		Size:                            44,
		VersionMadeBy:                   1,
		VersionNeededToExtract:          1,
		ThisDiskNum:                     0,
		DiskNumWithTheStartOfCentralDir: 0,
		TotalNumberOfEntriesOnThisDisk:  uint64(len(z.files)),
		TotalNumberOfEntries:            uint64(len(z.files)),
		CentralDirSize:                  centralDirSize,
		CentralDirOffset:                centralDirOffset,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __ZIP64_END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, record)
	return buf.Bytes()
}

type zip64EndOfCentralDirectoryLocator struct {
	EndOfCentralDirStartDiskNum uint32
	EndOfCentralDirOffset       uint64
	TotalNumberOfDisks          uint32
}

func encodeZip64EndOfCentralDirectoryLocator(endOfCentralDirOffset uint64) []byte {
	locator := zip64EndOfCentralDirectoryLocator{
		EndOfCentralDirStartDiskNum: 0,
		EndOfCentralDirOffset:       endOfCentralDirOffset,
		TotalNumberOfDisks:          0,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, locator)
	return buf.Bytes()
}
