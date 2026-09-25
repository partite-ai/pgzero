// Package memfs is an in-memory, POSIX-like filesystem for wacogo's
// wasi:filesystem implementation.
//
// Directory entries refer to nodes (inodes), so open files keep working
// after they are renamed or unlinked, and hard links share data. Files
// implement the capability interfaces wacogo looks for (OpenAter,
// io.WriterAt, preopens.RenameAter, ...), which gives guests the full set
// of operations Postgres needs without touching the host filesystem.
package memfs

import (
	"archive/tar"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
)

// FS is an in-memory filesystem.
type FS struct {
	// ns guards the namespace: every directory's entries and every
	// node's parent. File contents have their own per-node locks.
	ns   sync.RWMutex
	root *node
}

type node struct {
	mode fs.FileMode // fs.ModeDir, or 0 for a regular file (plus perm bits)

	mu      sync.RWMutex // guards data and modTime
	data    []byte
	modTime time.Time

	// Directories only, guarded by FS.ns.
	entries map[string]*node
	parent  *node
}

// New returns an empty filesystem.
func New() *FS {
	return &FS{root: newDir(nil)}
}

func newDir(parent *node) *node {
	n := &node{mode: fs.ModeDir | 0o700, modTime: time.Now(), entries: map[string]*node{}}
	n.parent = parent
	if parent == nil {
		n.parent = n
	}
	return n
}

func (n *node) isDir() bool { return n.mode.IsDir() }

func pathErr(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// Open implements fs.FS.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	return (&File{fs: f, node: f.root, name: "."}).OpenAt(name, 0, 0)
}

// resolve walks name from dir. With parentOnly it stops at the last
// component and returns its parent directory and base name. Must be
// called with f.ns held.
func (f *FS) resolve(dir *node, name string, parentOnly bool) (*node, string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, "", syscall.EPERM
	}
	parts := strings.Split(name, "/")
	var base string
	if parentOnly {
		// Trailing slashes don't name a different entry.
		for len(parts) > 1 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		base = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		if base == "" || base == "." || base == ".." {
			return nil, "", syscall.EINVAL
		}
	}
	cur := dir
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			if cur == f.root {
				return nil, "", syscall.EPERM // no escaping the preopen
			}
			cur = cur.parent
			continue
		}
		if !cur.isDir() {
			return nil, "", syscall.ENOTDIR
		}
		next, ok := cur.entries[p]
		if !ok {
			return nil, "", syscall.ENOENT
		}
		cur = next
	}
	if !cur.isDir() && (parentOnly || strings.HasSuffix(name, "/")) {
		return nil, "", syscall.ENOTDIR
	}
	return cur, base, nil
}

// File is an open file or directory.
type File struct {
	fs     *FS
	node   *node
	name   string
	flag   int
	offset int64
	dirPos int // ReadDir cursor
	mu     sync.Mutex
}

var (
	_ fs.ReadDirFile       = (*File)(nil)
	_ io.ReaderAt          = (*File)(nil)
	_ io.WriterAt          = (*File)(nil)
	_ io.Seeker            = (*File)(nil)
	_ preopens.OpenAter    = (*File)(nil)
	_ preopens.StatAter    = (*File)(nil)
	_ preopens.MkdirAter   = (*File)(nil)
	_ preopens.RmdirAter   = (*File)(nil)
	_ preopens.UnlinkAter  = (*File)(nil)
	_ preopens.Truncater   = (*File)(nil)
	_ preopens.Syncer      = (*File)(nil)
	_ preopens.Chtimeser   = (*File)(nil)
	_ preopens.ChtimesAter = (*File)(nil)
	_ preopens.RenameAter  = (*File)(nil)
	_ preopens.LinkAter    = (*File)(nil)
)

const accessModes = syscall.O_RDONLY | syscall.O_WRONLY | syscall.O_RDWR

func (f *File) writable() bool { return f.flag&accessModes != syscall.O_RDONLY }

// OpenAt opens name relative to this directory. flag takes the os.O_*
// values.
func (f *File) OpenAt(name string, flag int, perm fs.FileMode) (fs.File, error) {
	fsys := f.fs
	if flag&syscall.O_CREAT != 0 {
		fsys.ns.Lock()
		defer fsys.ns.Unlock()
	} else {
		fsys.ns.RLock()
		defer fsys.ns.RUnlock()
	}
	var n *node
	if flag&syscall.O_CREAT != 0 {
		dir, base, err := fsys.resolve(f.node, name, true)
		if err != nil {
			return nil, pathErr("open", name, err)
		}
		if existing, ok := dir.entries[base]; ok {
			if flag&syscall.O_EXCL != 0 {
				return nil, pathErr("open", name, syscall.EEXIST)
			}
			n = existing
		} else {
			n = &node{mode: perm.Perm() | 0o600, modTime: time.Now()}
			dir.entries[base] = n
			dir.touch()
		}
	} else {
		var err error
		if n, _, err = fsys.resolve(f.node, name, false); err != nil {
			return nil, pathErr("open", name, err)
		}
	}
	nf := &File{fs: fsys, node: n, name: path.Base(name), flag: flag}
	if n.isDir() {
		if nf.writable() || flag&syscall.O_TRUNC != 0 {
			return nil, pathErr("open", name, syscall.EISDIR)
		}
		return nf, nil
	}
	if flag&syscall.O_TRUNC != 0 && nf.writable() {
		n.mu.Lock()
		n.data = n.data[:0]
		n.modTime = time.Now()
		n.mu.Unlock()
	}
	return nf, nil
}

func (n *node) touch() {
	n.mu.Lock()
	n.modTime = time.Now()
	n.mu.Unlock()
}

// Stat implements fs.File.
func (f *File) Stat() (fs.FileInfo, error) { return f.node.info(f.name), nil }

// StatAt stats name relative to this directory.
func (f *File) StatAt(name string) (fs.FileInfo, error) {
	f.fs.ns.RLock()
	defer f.fs.ns.RUnlock()
	n, _, err := f.fs.resolve(f.node, name, false)
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	return n.info(path.Base(name)), nil
}

func (f *File) Close() error { return nil }

func (f *File) Read(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.ReadAt(b, f.offset)
	f.offset += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (f *File) ReadAt(b []byte, off int64) (int, error) {
	if f.node.isDir() {
		return 0, pathErr("read", f.name, syscall.EISDIR)
	}
	if off < 0 {
		return 0, pathErr("read", f.name, syscall.EINVAL)
	}
	f.node.mu.RLock()
	defer f.node.mu.RUnlock()
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(b, f.node.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (f *File) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flag&syscall.O_APPEND != 0 {
		f.node.mu.RLock()
		f.offset = int64(len(f.node.data))
		f.node.mu.RUnlock()
	}
	n, err := f.WriteAt(b, f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *File) WriteAt(b []byte, off int64) (int, error) {
	if f.node.isDir() {
		return 0, pathErr("write", f.name, syscall.EISDIR)
	}
	if !f.writable() {
		return 0, pathErr("write", f.name, syscall.EBADF)
	}
	if off < 0 {
		return 0, pathErr("write", f.name, syscall.EINVAL)
	}
	n := f.node
	n.mu.Lock()
	defer n.mu.Unlock()
	end := off + int64(len(b))
	if end > int64(len(n.data)) {
		n.data = grow(n.data, int(end))
	}
	copy(n.data[off:], b)
	n.modTime = time.Now()
	return len(b), nil
}

// grow extends data to size, zero-filling, with amortized reallocation.
func grow(data []byte, size int) []byte {
	if size <= cap(data) {
		old := len(data)
		data = data[:size]
		clear(data[old:])
		return data
	}
	c := max(size, 2*cap(data))
	nd := make([]byte, size, c)
	copy(nd, data)
	return nd
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		f.node.mu.RLock()
		offset += int64(len(f.node.data))
		f.node.mu.RUnlock()
	default:
		return 0, pathErr("seek", f.name, syscall.EINVAL)
	}
	if offset < 0 {
		return 0, pathErr("seek", f.name, syscall.EINVAL)
	}
	f.offset = offset
	return offset, nil
}

func (f *File) Truncate(size int64) error {
	if f.node.isDir() {
		return pathErr("truncate", f.name, syscall.EISDIR)
	}
	if size < 0 {
		return pathErr("truncate", f.name, syscall.EINVAL)
	}
	n := f.node
	n.mu.Lock()
	defer n.mu.Unlock()
	if int(size) <= len(n.data) {
		n.data = n.data[:size]
	} else {
		n.data = grow(n.data, int(size))
	}
	n.modTime = time.Now()
	return nil
}

// Sync is a no-op: there is nothing to write back.
func (f *File) Sync() error { return nil }

func (f *File) Chtimes(_, mtime time.Time) error {
	f.node.mu.Lock()
	f.node.modTime = mtime
	f.node.mu.Unlock()
	return nil
}

func (f *File) ChtimesAt(name string, _, mtime time.Time) error {
	f.fs.ns.RLock()
	n, _, err := f.fs.resolve(f.node, name, false)
	f.fs.ns.RUnlock()
	if err != nil {
		return pathErr("chtimes", name, err)
	}
	n.mu.Lock()
	n.modTime = mtime
	n.mu.Unlock()
	return nil
}

// ReadDir implements fs.ReadDirFile, in name order.
func (f *File) ReadDir(count int) ([]fs.DirEntry, error) {
	if !f.node.isDir() {
		return nil, pathErr("readdir", f.name, syscall.ENOTDIR)
	}
	f.fs.ns.RLock()
	names := make([]string, 0, len(f.node.entries))
	for name := range f.node.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	nodes := make([]*node, len(names))
	for i, name := range names {
		nodes[i] = f.node.entries[name]
	}
	f.fs.ns.RUnlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirPos >= len(names) {
		if count > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
	end := len(names)
	if count > 0 && f.dirPos+count < end {
		end = f.dirPos + count
	}
	out := make([]fs.DirEntry, 0, end-f.dirPos)
	for i := f.dirPos; i < end; i++ {
		out = append(out, fs.FileInfoToDirEntry(nodes[i].info(names[i])))
	}
	f.dirPos = end
	return out, nil
}

func (f *File) MkdirAt(name string, perm fs.FileMode) error {
	f.fs.ns.Lock()
	defer f.fs.ns.Unlock()
	dir, base, err := f.fs.resolve(f.node, name, true)
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	if _, ok := dir.entries[base]; ok {
		return pathErr("mkdir", name, syscall.EEXIST)
	}
	d := newDir(dir)
	d.mode = fs.ModeDir | perm.Perm() | 0o700
	dir.entries[base] = d
	dir.touch()
	return nil
}

func (f *File) RmdirAt(name string) error {
	f.fs.ns.Lock()
	defer f.fs.ns.Unlock()
	dir, base, err := f.fs.resolve(f.node, name, true)
	if err != nil {
		return pathErr("rmdir", name, err)
	}
	n, ok := dir.entries[base]
	switch {
	case !ok:
		return pathErr("rmdir", name, syscall.ENOENT)
	case !n.isDir():
		return pathErr("rmdir", name, syscall.ENOTDIR)
	case len(n.entries) > 0:
		return pathErr("rmdir", name, syscall.ENOTEMPTY)
	}
	delete(dir.entries, base)
	dir.touch()
	return nil
}

func (f *File) UnlinkAt(name string) error {
	f.fs.ns.Lock()
	defer f.fs.ns.Unlock()
	dir, base, err := f.fs.resolve(f.node, name, true)
	if err != nil {
		return pathErr("unlink", name, err)
	}
	n, ok := dir.entries[base]
	switch {
	case !ok:
		return pathErr("unlink", name, syscall.ENOENT)
	case n.isDir():
		return pathErr("unlink", name, syscall.EISDIR)
	}
	delete(dir.entries, base)
	dir.touch()
	return nil
}

// ReadlinkAt: there are no symlinks.
func (f *File) ReadlinkAt(name string) (string, error) {
	return "", pathErr("readlink", name, syscall.EINVAL)
}

// otherDir returns newDir as a directory of this filesystem, or EXDEV.
func (f *File) otherDir(newDir fs.File) (*node, error) {
	nd, ok := preopens.As[*File](newDir)
	if !ok || nd.fs != f.fs {
		return nil, syscall.EXDEV
	}
	if !nd.node.isDir() {
		return nil, syscall.ENOTDIR
	}
	return nd.node, nil
}

// RenameAt implements preopens.RenameAter with rename(2) semantics.
func (f *File) RenameAt(oldName string, newDir fs.File, newName string) error {
	nd, err := f.otherDir(newDir)
	if err != nil {
		return pathErr("rename", oldName, err)
	}
	fsys := f.fs
	fsys.ns.Lock()
	defer fsys.ns.Unlock()
	odir, obase, err := fsys.resolve(f.node, oldName, true)
	if err != nil {
		return pathErr("rename", oldName, err)
	}
	ndir, nbase, err := fsys.resolve(nd, newName, true)
	if err != nil {
		return pathErr("rename", newName, err)
	}
	n, ok := odir.entries[obase]
	if !ok {
		return pathErr("rename", oldName, syscall.ENOENT)
	}
	if target, ok := ndir.entries[nbase]; ok {
		if target == n {
			return nil
		}
		switch {
		case n.isDir() && !target.isDir():
			return pathErr("rename", newName, syscall.ENOTDIR)
		case !n.isDir() && target.isDir():
			return pathErr("rename", newName, syscall.EISDIR)
		case target.isDir() && len(target.entries) > 0:
			return pathErr("rename", newName, syscall.ENOTEMPTY)
		}
	}
	if n.isDir() {
		// A directory can't move into its own subtree.
		for d := ndir; ; d = d.parent {
			if d == n {
				return pathErr("rename", newName, syscall.EINVAL)
			}
			if d == fsys.root {
				break
			}
		}
		n.parent = ndir
	}
	delete(odir.entries, obase)
	ndir.entries[nbase] = n
	odir.touch()
	ndir.touch()
	return nil
}

// LinkAt implements preopens.LinkAter: a hard link to a regular file.
func (f *File) LinkAt(oldName string, newDir fs.File, newName string) error {
	nd, err := f.otherDir(newDir)
	if err != nil {
		return pathErr("link", oldName, err)
	}
	fsys := f.fs
	fsys.ns.Lock()
	defer fsys.ns.Unlock()
	n, _, err := fsys.resolve(f.node, oldName, false)
	if err != nil {
		return pathErr("link", oldName, err)
	}
	if n.isDir() {
		return pathErr("link", oldName, syscall.EPERM)
	}
	ndir, nbase, err := fsys.resolve(nd, newName, true)
	if err != nil {
		return pathErr("link", newName, err)
	}
	if _, ok := ndir.entries[nbase]; ok {
		return pathErr("link", newName, syscall.EEXIST)
	}
	ndir.entries[nbase] = n
	ndir.touch()
	return nil
}

// fileInfo is a snapshot of a node's metadata.
type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (n *node) info(name string) fs.FileInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return fileInfo{name: name, size: int64(len(n.data)), mode: n.mode, modTime: n.modTime}
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.modTime }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

// Clone returns an independent deep copy of the filesystem. Hard links
// within it are preserved.
func (f *FS) Clone() *FS {
	f.ns.RLock()
	defer f.ns.RUnlock()
	copies := map[*node]*node{}
	var clone func(n, parent *node) *node
	clone = func(n, parent *node) *node {
		if c, ok := copies[n]; ok {
			return c
		}
		n.mu.RLock()
		c := &node{mode: n.mode, modTime: n.modTime}
		if n.data != nil {
			c.data = append([]byte(nil), n.data...)
		}
		n.mu.RUnlock()
		copies[n] = c
		if n.isDir() {
			c.parent = parent
			c.entries = make(map[string]*node, len(n.entries))
			for name, child := range n.entries {
				c.entries[name] = clone(child, c)
			}
		}
		return c
	}
	root := clone(f.root, nil)
	root.parent = root
	return &FS{root: root}
}

// AddTar adds the directories and regular files of a tar archive.
func (f *FS) AddTar(r io.Reader) error {
	root := &File{fs: f, node: f.root, name: "."}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		if name == "." || name == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := f.mkdirAll(name, fs.FileMode(h.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if dir := path.Dir(name); dir != "." {
				if err := f.mkdirAll(dir, 0o700); err != nil {
					return err
				}
			}
			file, err := root.OpenAt(name, syscall.O_CREAT|syscall.O_TRUNC|syscall.O_WRONLY, fs.FileMode(h.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(file.(io.Writer), tr); err != nil {
				return err
			}
			file.(*File).node.modTime = h.ModTime
		default:
			return errors.New("memfs: unsupported tar entry type for " + h.Name)
		}
	}
}

func (f *FS) mkdirAll(name string, perm fs.FileMode) error {
	root := &File{fs: f, node: f.root, name: "."}
	cur := ""
	for _, p := range strings.Split(name, "/") {
		cur = path.Join(cur, p)
		if err := root.MkdirAt(cur, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}
