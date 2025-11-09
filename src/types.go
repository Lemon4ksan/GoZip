package gozip

import (
	"bytes"
	"encoding/binary"
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

// HostSystem represents the host system on which the ZIP file was created
type HostSystem uint8

// Supported host systems according to ZIP specification
const (
	HostSystemFAT       HostSystem = 0  // MS-DOS and OS/2 (FAT / VFAT / FAT32 file systems)
	HostSystemAmiga     HostSystem = 1  // Amiga
	HostSystemOpenVMS   HostSystem = 2  // OpenVMS
	HostSystemUNIX      HostSystem = 3  // UNIX
	HostSystemVMCMS     HostSystem = 4  // VM/CMS
	HostSystemAtariST   HostSystem = 5  // Atari ST
	HostSystemOS2HPFS   HostSystem = 6  // OS/2 H.P.F.S.
	HostSystemMacintosh HostSystem = 7  // Macintosh
	HostSystemZSystem   HostSystem = 8  // Z-System
	HostSystemCPM       HostSystem = 9  // CP/M
	HostSystemNTFS      HostSystem = 10 // Windows NTFS
	HostSystemMVS       HostSystem = 11 // MVS (OS/390 - Z/OS)
	HostSystemVSE       HostSystem = 12 // VSE
	HostSystemAcornRisc HostSystem = 13 // Acorn Risc
	HostSystemVFAT      HostSystem = 14 // VFAT
	HostSystemAltMVS    HostSystem = 15 // alternate MVS
	HostSystemBeOS      HostSystem = 16 // BeOS
	HostSystemTandem    HostSystem = 17 // Tandem
	HostSystemOS400     HostSystem = 18 // OS/400
	HostSystemDarwin    HostSystem = 19 // OS X (Darwin)
	// 20-255: unused
)

// String representation of HostSystem for debugging
func (h HostSystem) String() string {
	names := map[HostSystem]string{
		HostSystemFAT:       "MS-DOS/OS2 (FAT)",
		HostSystemAmiga:     "Amiga",
		HostSystemOpenVMS:   "OpenVMS",
		HostSystemUNIX:      "UNIX",
		HostSystemVMCMS:     "VM/CMS",
		HostSystemAtariST:   "Atari ST",
		HostSystemOS2HPFS:   "OS/2 HPFS",
		HostSystemMacintosh: "Macintosh",
		HostSystemZSystem:   "Z-System",
		HostSystemCPM:       "CP/M",
		HostSystemNTFS:      "Windows NTFS",
		HostSystemMVS:       "MVS (OS/390 - Z/OS)",
		HostSystemVSE:       "VSE",
		HostSystemAcornRisc: "Acorn Risc",
		HostSystemVFAT:      "VFAT",
		HostSystemAltMVS:    "Alternate MVS",
		HostSystemBeOS:      "BeOS",
		HostSystemTandem:    "Tandem",
		HostSystemOS400:     "OS/400",
		HostSystemDarwin:    "OS X (Darwin)",
	}

	if name, exists := names[h]; exists {
		return name
	}
	return "Unknown"
}

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
	for _, entry := range f.extraField {
		if entry.Tag == __ZIP64_EXTRA_FIELD {
			buf.Write(entry.Data)
			break
		}
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
	buf.WriteString(f.comment)
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
func encodeEndOfCentralDirRecord(z *Zip, centralDirSize uint32, centralDirOffset uint32) []byte {
	record := endOfCentralDirectory{
		ThisDiskNum:                     0,
		DiskNumWithTheStartOfCentralDir: 0,
		TotalNumberOfEntriesOnThisDisk:  uint16(len(z.files)),
		TotalNumberOfEntries:            uint16(len(z.files)),
		CentralDirSize:                  centralDirSize,
		CentralDirOffset:                centralDirOffset,
		CommentLength:                   uint16(len(z.comment)),
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, __END_OF_CENTRAL_DIRECTORY_SIGNATURE)
	binary.Write(buf, binary.LittleEndian, record)
	buf.WriteString(z.comment)
	return buf.Bytes()
}
