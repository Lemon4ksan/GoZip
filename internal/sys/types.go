package sys

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

// IsWindows returns true if the host system is a Windows variant.
// In the wild, 99% of Windows archives use HostSystemFAT (0),
// but HostSystemNTFS (10) and HostSystemVFAT (14) are also possible per spec.
func (h HostSystem) IsWindows() bool {
	switch h {
	case HostSystemFAT, HostSystemNTFS, HostSystemVFAT:
		return true
	default:
		return false
	}
}

// IsUnix returns true if the host system is a Unix variant.
// Most modern archives use HostSystemUNIX (3), but macOS might theoretically use HostSystemDarwin (19).
func (h HostSystem) IsUnix() bool {
	switch h {
	case HostSystemUNIX, HostSystemDarwin:
		return true
	default:
		return false
	}
}

// String representation of HostSystem for debugging
func (h HostSystem) String() string {
	names := map[HostSystem]string{
		HostSystemFAT:       "FAT",
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

// Unix constants for file types (standard POSIX)
const (
	S_IFMT   = 0170000 // Mask for file type
	S_IFSOCK = 0140000 // Socket
	S_IFLNK  = 0120000 // Symbolic link
	S_IFREG  = 0100000 // Regular file
	S_IFBLK  = 0060000 // Block device
	S_IFDIR  = 0040000 // Directory
	S_IFCHR  = 0020000 // Character device
	S_IFIFO  = 0010000 // FIFO / Named pipe
	S_ISUID  = 0004000 // Set-user-ID bit
	S_ISGID  = 0002000 // Set-group-ID bit
	S_ISVTX  = 0001000 // Sticky bit
)
