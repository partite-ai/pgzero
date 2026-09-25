package memfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"testing/fstest"
)

func root(f *FS) *File { return &File{fs: f, node: f.root, name: "."} }

func create(t *testing.T, dir *File, name, content string) *File {
	t.Helper()
	fl, err := dir.OpenAt(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := fl.(*File).Write([]byte(content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return fl.(*File)
}

func read(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func wantErrno(t *testing.T, err error, errno syscall.Errno) {
	t.Helper()
	if !errors.Is(err, errno) {
		t.Fatalf("got error %v, want %v", err, errno)
	}
}

func TestFSConformance(t *testing.T) {
	f := New()
	r := root(f)
	if err := r.MkdirAt("a", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAt("a/b", 0o700); err != nil {
		t.Fatal(err)
	}
	create(t, r, "top.txt", "top")
	create(t, r, "a/one.txt", "one")
	create(t, r, "a/b/two.txt", "two")
	if err := fstest.TestFS(f, "top.txt", "a/one.txt", "a/b/two.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestReadWrite(t *testing.T) {
	f := New()
	fl := create(t, root(f), "f", "hello")
	if _, err := fl.WriteAt([]byte("X"), 10); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f, "f"); got != "hello\x00\x00\x00\x00\x00X" {
		t.Fatalf("content %q", got)
	}
	if err := fl.Truncate(2); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	n, err := fl.ReadAt(buf, 0)
	if n != 2 || err != io.EOF || string(buf[:n]) != "he" {
		t.Fatalf("ReadAt = %d %v %q", n, err, buf[:n])
	}
	if _, err := fl.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f, "f"); got != "hey" {
		t.Fatalf("content %q", got)
	}
}

func TestOpenFlags(t *testing.T) {
	f := New()
	r := root(f)
	create(t, r, "f", "data")
	_, err := r.OpenAt("f", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	wantErrno(t, err, syscall.EEXIST)
	_, err = r.OpenAt("missing", os.O_RDONLY, 0)
	wantErrno(t, err, syscall.ENOENT)
	_, err = r.OpenAt("f/x", os.O_RDONLY, 0)
	wantErrno(t, err, syscall.ENOTDIR)
	ro, err := r.OpenAt("f", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ro.(*File).WriteAt([]byte("x"), 0)
	wantErrno(t, err, syscall.EBADF)
	if err := r.MkdirAt("d", 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = r.OpenAt("d", os.O_RDWR, 0)
	wantErrno(t, err, syscall.EISDIR)
	ap, err := r.OpenAt("f", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	ap.(*File).Write([]byte("!"))
	if got := read(t, f, "f"); got != "data!" {
		t.Fatalf("append: %q", got)
	}
}

func TestUnlinkWhileOpen(t *testing.T) {
	f := New()
	r := root(f)
	fl := create(t, r, "f", "still here")
	if err := r.UnlinkAt("f"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StatAt("f"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after unlink: %v", err)
	}
	buf := make([]byte, 20)
	n, _ := fl.ReadAt(buf, 0)
	if string(buf[:n]) != "still here" {
		t.Fatalf("open file lost its data: %q", buf[:n])
	}
}

func TestRename(t *testing.T) {
	f := New()
	r := root(f)
	for _, d := range []string{"a", "a/sub", "b", "full"} {
		if err := r.MkdirAt(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	create(t, r, "full/x", "x")
	open := create(t, r, "a/f", "moved")
	create(t, r, "b/f", "replaced")

	// Replaces an existing file; the open handle follows the file.
	if err := r.RenameAt("a/f", r, "b/f"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f, "b/f"); got != "moved" {
		t.Fatalf("content %q", got)
	}
	open.Write([]byte("!"))
	if got := read(t, f, "b/f"); got != "moved!" {
		t.Fatalf("handle after rename: %q", got)
	}

	wantErrno(t, r.RenameAt("missing", r, "x"), syscall.ENOENT)
	wantErrno(t, r.RenameAt("a", r, "a/sub/inside"), syscall.EINVAL)
	wantErrno(t, r.RenameAt("a", r, "full"), syscall.ENOTEMPTY)
	wantErrno(t, r.RenameAt("b/f", r, "a"), syscall.EISDIR)
	wantErrno(t, r.RenameAt("a", r, "b/f"), syscall.ENOTDIR)

	// Directory rename, relative to a subdirectory handle, with "..".
	sub, err := r.OpenAt("b", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.(*File).RenameAt("../a", r, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StatAt("c/sub"); err != nil {
		t.Fatalf("renamed dir: %v", err)
	}
	if _, err := r.StatAt("c/sub/.."); err != nil {
		t.Fatalf("parent of renamed dir: %v", err)
	}

	// Another filesystem is another device.
	other := root(New())
	wantErrno(t, r.RenameAt("b/f", other, "f"), syscall.EXDEV)
}

func TestLink(t *testing.T) {
	f := New()
	r := root(f)
	create(t, r, "f", "shared")
	if err := r.LinkAt("f", r, "g"); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, r.LinkAt("f", r, "g"), syscall.EEXIST)
	g, err := r.OpenAt("g", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	g.(*File).Write([]byte("!"))
	if got := read(t, f, "f"); got != "shared!" {
		t.Fatalf("link does not share data: %q", got)
	}
}

func TestDirs(t *testing.T) {
	f := New()
	r := root(f)
	if err := r.MkdirAt("d", 0o700); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, r.MkdirAt("d", 0o700), syscall.EEXIST)
	create(t, r, "d/f", "")
	wantErrno(t, r.RmdirAt("d"), syscall.ENOTEMPTY)
	wantErrno(t, r.UnlinkAt("d"), syscall.EISDIR)
	wantErrno(t, r.RmdirAt("d/f"), syscall.ENOTDIR)
	_, err := r.StatAt("../escape")
	wantErrno(t, err, syscall.EPERM)
	if err := r.UnlinkAt("d/f"); err != nil {
		t.Fatal(err)
	}
	if err := r.RmdirAt("d"); err != nil {
		t.Fatal(err)
	}
}

func TestCloneAndTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o700})
	tw.WriteHeader(&tar.Header{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 3})
	tw.Write([]byte("abc"))
	tw.WriteHeader(&tar.Header{Name: "implicit/parent/file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1})
	tw.Write([]byte("z"))
	tw.Close()

	f := New()
	if err := f.AddTar(&buf); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f, "dir/file"); got != "abc" {
		t.Fatalf("tar content %q", got)
	}
	if got := read(t, f, "implicit/parent/file"); got != "z" {
		t.Fatalf("tar content %q", got)
	}

	c := f.Clone()
	fl, err := root(c).OpenAt("dir/file", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	fl.(*File).Write([]byte("changed"))
	if got := read(t, f, "dir/file"); got != "abc" {
		t.Fatalf("clone shares data with original: %q", got)
	}
	if got := read(t, c, "dir/file"); got != "changed" {
		t.Fatalf("clone content %q", got)
	}
}
