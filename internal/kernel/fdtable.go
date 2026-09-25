package kernel

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
)

// file is something a host descriptor can refer to. All operations are
// non-blocking; the kernel implements blocking on top of poll and changed.
type file interface {
	// read reads into b. It returns (0, 0) at end of file and
	// (0, EAGAIN) when no data is available yet.
	read(b []byte) (int, int32)
	// write writes from b, or returns (0, EAGAIN) if it would block.
	write(b []byte) (int, int32)
	// poll reports current readiness as POLL* bits.
	poll() int16
	// changed returns a channel that is closed at the next change in
	// readiness (nil if readiness never changes).
	changed() <-chan struct{}
	// release is called when the last descriptor referring to the open
	// file is closed.
	release()
}

// openFile is an open file description: shared by descriptors created
// with dup or inherited across spawn, together with its status flags.
type openFile struct {
	f     file
	flags atomic.Int32 // O_NONBLOCK
	refs  atomic.Int32
}

func newOpenFile(f file, flags int32) *openFile {
	of := &openFile{f: f}
	of.flags.Store(flags)
	return of
}

func (of *openFile) nonblock() bool { return of.flags.Load()&abi.O_NONBLOCK != 0 }

func (of *openFile) ref() *openFile {
	of.refs.Add(1)
	return of
}

func (of *openFile) unref() {
	if of.refs.Add(-1) == 0 {
		of.f.release()
	}
}

type fdEntry struct {
	of      *openFile
	cloexec bool
}

// fdTable is a process's host descriptors: stdio (0-2) and numbers from
// abi.FDBase up.
type fdTable struct {
	mu  sync.Mutex
	fds map[int32]*fdEntry
}

func newFDTable() *fdTable {
	return &fdTable{fds: map[int32]*fdEntry{}}
}

// install puts of at fd (closing whatever was there) and takes a reference.
func (t *fdTable) install(fd int32, of *openFile, cloexec bool) {
	t.mu.Lock()
	old := t.fds[fd]
	t.fds[fd] = &fdEntry{of: of.ref(), cloexec: cloexec}
	t.mu.Unlock()
	if old != nil {
		old.of.unref()
	}
}

// add installs of at the lowest free number >= abi.FDBase.
func (t *fdTable) add(of *openFile, cloexec bool) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	fd := int32(abi.FDBase)
	for t.fds[fd] != nil {
		fd++
	}
	t.fds[fd] = &fdEntry{of: of.ref(), cloexec: cloexec}
	return fd
}

func (t *fdTable) get(fd int32) (*openFile, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fds[fd]
	if e == nil {
		return nil, false
	}
	return e.of, true
}

func (t *fdTable) close(fd int32) int32 {
	t.mu.Lock()
	e := t.fds[fd]
	delete(t.fds, fd)
	t.mu.Unlock()
	if e == nil {
		return -abi.EBADF
	}
	e.of.unref()
	return 0
}

func (t *fdTable) getCloexec(fd int32) (bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fds[fd]
	if e == nil {
		return false, false
	}
	return e.cloexec, true
}

func (t *fdTable) setCloexec(fd int32, cloexec bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fds[fd]
	if e == nil {
		return false
	}
	e.cloexec = cloexec
	return true
}

// cloneForExec returns the table a spawned child starts with: every
// descriptor not marked close-on-exec, sharing the open files.
func (t *fdTable) cloneForExec() *fdTable {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := newFDTable()
	for fd, e := range t.fds {
		if !e.cloexec {
			c.fds[fd] = &fdEntry{of: e.of.ref()}
		}
	}
	return c
}

func (t *fdTable) closeAll() {
	t.mu.Lock()
	fds := t.fds
	t.fds = map[int32]*fdEntry{}
	t.mu.Unlock()
	keys := make([]int32, 0, len(fds))
	for fd := range fds {
		keys = append(keys, fd)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, fd := range keys {
		fds[fd].of.unref()
	}
}

// io runs a non-blocking operation on of, blocking (interruptibly) until
// it can make progress unless the file is in non-blocking mode.
func (p *Process) io(of *openFile, op func() (int, int32)) (int32, error) {
	for {
		ch := of.f.changed()
		n, errno := op()
		if errno == 0 {
			return int32(n), nil
		}
		if errno != abi.EAGAIN || of.nonblock() {
			return -errno, nil
		}
		switch p.block(time.Time{}, ch) {
		case interrupted:
			return -abi.EINTR, nil
		case killed:
			return 0, errKilled
		}
	}
}
