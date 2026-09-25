// Package osfs exposes a host directory to wacogo's wasi:filesystem
// implementation with full read-write support.
//
// wacogo adapts an fs.FS and discovers extra capabilities through optional
// interfaces on the files it opens (OpenAter, MkdirAter, io.WriterAt, ...).
// Files here are real *os.File values, which also lets wacogo implement
// rename and link with renameat(2)/linkat(2) on their descriptors.
package osfs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Dir is a directory tree rooted at a host path.
type Dir string

// Open implements fs.FS.
func (d Dir) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return openFile(filepath.Join(string(d), filepath.FromSlash(name)), os.O_RDONLY, 0)
}

// File is an open file or directory.
type File struct {
	*os.File
	path string // host path, for the *At methods
}

func openFile(p string, flag int, perm fs.FileMode) (*File, error) {
	f, err := os.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return &File{File: f, path: p}, nil
}

// resolve joins a guest path relative to this directory. Guest paths are
// already confined to the preopen by wasi-libc's path resolution; we only
// reject absolute paths and escapes above the directory as a safeguard.
func (f *File) resolve(name string) (string, error) {
	clean := path.Clean(name)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fs.ErrPermission
	}
	return filepath.Join(f.path, filepath.FromSlash(clean)), nil
}

// OpenAt opens name relative to this directory.
func (f *File) OpenAt(name string, flag int, perm fs.FileMode) (fs.File, error) {
	p, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	nf, err := openFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return nf, nil
}

// StatAt stats name relative to this directory without following a final
// symlink (callers that want to follow it open the file instead).
func (f *File) StatAt(name string) (fs.FileInfo, error) {
	p, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

// ReadlinkAt reads the symlink name.
func (f *File) ReadlinkAt(name string) (string, error) {
	p, err := f.resolve(name)
	if err != nil {
		return "", err
	}
	return os.Readlink(p)
}

// MkdirAt creates the directory name.
func (f *File) MkdirAt(name string, perm fs.FileMode) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	return os.Mkdir(p, perm)
}

// RmdirAt removes the empty directory name.
func (f *File) RmdirAt(name string) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err != nil {
		return err
	} else if !fi.IsDir() {
		return &fs.PathError{Op: "rmdir", Path: name, Err: syscall.ENOTDIR}
	}
	return os.Remove(p)
}

// UnlinkAt removes the non-directory name.
func (f *File) UnlinkAt(name string) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err != nil {
		return err
	} else if fi.IsDir() {
		return &fs.PathError{Op: "unlink", Path: name, Err: syscall.EISDIR}
	}
	return os.Remove(p)
}

// SymlinkAt creates a symlink named linkName pointing at target.
func (f *File) SymlinkAt(target, linkName string) error {
	p, err := f.resolve(linkName)
	if err != nil {
		return err
	}
	return os.Symlink(target, p)
}

// Chtimes sets this file's times.
func (f *File) Chtimes(atime, mtime time.Time) error {
	return os.Chtimes(f.path, atime, mtime)
}

// ChtimesAt sets the times of name.
func (f *File) ChtimesAt(name string, atime, mtime time.Time) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	return os.Chtimes(p, atime, mtime)
}
