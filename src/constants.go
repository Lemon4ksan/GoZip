package gozip

// CompressionMethod represents the compression algorithm used for a file in the ZIP archive
type CompressionMethod uint16

// Supported compression methods according to ZIP specification
const (
	Stored   CompressionMethod = 0 // No compression - file stored as-is
	Deflated CompressionMethod = 8 // DEFLATE compression (most common)
)

// Compression levels for DEFLATE algorithm
const (
	DeflateNormal    = 6 // Default compression level (good balance between speed and ratio)
	DeflateMaximum   = 9 // Maximum compression (best ratio, slowest speed)
	DeflateFast      = 3 // Fast compression (lower ratio, faster speed)
	DeflateSuperFast = 1 // Super fast compression (lowest ratio, fastest speed)
)

// EncryptionMethod represents the encryption algorithm used for file protection
type EncryptionMethod uint16

// Supported encryption methods
const (
	NotEncrypted EncryptionMethod = 0 // No encryption - file stored in plaintext
)

// FileSystemType represents the type of file system on which the ZIP file was created
type FileSystemType int

const (
    FileSystemUnknown FileSystemType = iota
    FileSystemFAT
    FileSystemNTFS
    FileSystemEXT4
    FileSystemAPFS
    FileSystemHFSPlus
    FileSystemZFS
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