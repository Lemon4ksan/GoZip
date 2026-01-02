package gozip

import (
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

var (
	_ fs.FS        = (*zipFS)(nil)
	_ fs.StatFS    = (*zipFS)(nil)
	_ fs.ReadDirFS = (*zipFS)(nil)
)

type zipFS struct {
	z *Zip
}

// Open implements fs.FS, allowing the Zip archive to be used as a read-only filesystem.
func (zfs *zipFS) Open(name string) (fs.File, error) {
	entry, err := zfs.getFileEntry(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if entry.isDir {
		return &fsDir{entry: entry, z: zfs.z}, nil
	}

	fsFile, err := newFsFile(entry)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	return fsFile, nil
}

// Stat implements fs.StatFS.
func (zfs *zipFS) Stat(name string) (fs.FileInfo, error) {
	entry, err := zfs.getFileEntry(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return fileInfoAdapter{entry}, nil
}

// ReadDir implements fs.ReadDirFS.
func (zfs *zipFS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := zfs.Open(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	defer file.Close()

	dir, ok := file.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	return dir.ReadDir(-1)
}

// getFileEntry is a helper function to get the File entry for a given name.
// It handles the root directory, explicit files, and implicit directories.
func (zfs *zipFS) getFileEntry(name string) (*File, error) {
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}

	zfs.z.mu.RLock()
	defer zfs.z.mu.RUnlock()

	if name == "." {
		return &File{
			name:    ".",
			isDir:   true,
			mode:    fs.ModeDir | 0755,
			modTime: time.Now(),
		}, nil
	}

	if f, err := zfs.z.File(name); err == nil {
		return f, nil
	}

	if zfs.hasImplicitDir(name) {
		return &File{
			name:    name,
			isDir:   true,
			mode:    fs.ModeDir | 0755,
			modTime: time.Now(),
		}, nil
	}

	return nil, fs.ErrNotExist
}

func (zfs *zipFS) hasImplicitDir(name string) bool {
	prefix := name + "/"
	for _, f := range zfs.z.files {
		if strings.HasPrefix(f.getFilename(), prefix) {
			return true
		}
	}
	return false
}

// fsFile wraps a regular compressed file to satisfy fs.File
type fsFile struct {
	entry *File
	rc    io.ReadCloser
}

func newFsFile(f *File) (*fsFile, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return &fsFile{entry: f, rc: rc}, nil
}

func (f *fsFile) Stat() (fs.FileInfo, error) { return fileInfoAdapter{f.entry}, nil }
func (f *fsFile) Read(b []byte) (int, error) { return f.rc.Read(b) }
func (f *fsFile) Close() error               { return f.rc.Close() }

// fsDir wraps a directory entry to satisfy fs.ReadDirFile
type fsDir struct {
	entry *File
	z     *Zip
}

func (d *fsDir) Stat() (fs.FileInfo, error) { return fileInfoAdapter{d.entry}, nil }
func (d *fsDir) Close() error               { return nil }
func (d *fsDir) Read(b []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.entry.name, Err: fs.ErrInvalid}
}

// ReadDir searches file list to finds current dir children.
func (d *fsDir) ReadDir(n int) ([]fs.DirEntry, error) {
	d.z.mu.RLock()
	defer d.z.mu.RUnlock()

	dirPath := d.entry.name
	if dirPath == "." {
		dirPath = ""
	} else if !strings.HasSuffix(dirPath, "/") {
		dirPath += "/"
	}

	seen := make(map[string]bool)
	var entries []fs.DirEntry

	for _, f := range d.z.files {
		filename := f.getFilename()
		if !strings.HasPrefix(filename, dirPath) {
			continue
		}

		rel := strings.TrimPrefix(filename, dirPath)
		if rel == "" {
			continue
		}

		parts := strings.SplitN(rel, "/", 2)
		childName := parts[0]

		if seen[childName] {
			continue
		}
		seen[childName] = true

		isDir := len(parts) > 1 || f.isDir
		entries = append(entries, fsDirEntryAdapter{
			name:  childName,
			isDir: isDir,
			info:  fileInfoAdapter{f},
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	if n <= 0 {
		return entries, nil
	}

	if len(entries) <= n {
		return entries, io.EOF
	}

	return entries[:n], nil
}

type fileInfoAdapter struct{ f *File }

func (i fileInfoAdapter) Name() string       { return path.Base(i.f.name) }
func (i fileInfoAdapter) Size() int64        { return i.f.uncompressedSize }
func (i fileInfoAdapter) Mode() fs.FileMode  { return i.f.mode }
func (i fileInfoAdapter) ModTime() time.Time { return i.f.modTime }
func (i fileInfoAdapter) IsDir() bool        { return i.f.isDir }
func (i fileInfoAdapter) Sys() interface{}   { return nil }

type fsDirEntryAdapter struct {
	name  string
	isDir bool
	info  fs.FileInfo
}

func (e fsDirEntryAdapter) Name() string               { return e.name }
func (e fsDirEntryAdapter) IsDir() bool                { return e.isDir }
func (e fsDirEntryAdapter) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e fsDirEntryAdapter) Info() (fs.FileInfo, error) { return e.info, nil }
