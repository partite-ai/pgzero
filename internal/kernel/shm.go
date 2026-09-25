package kernel

import (
	"sync"

	"github.com/partite-ai/pgzero/internal/abi"
	"github.com/partite-ai/pgzero/internal/vmem"
)

// segment is a shared memory object: an anonymous segment (Postgres's
// main shared memory) or a named POSIX shared memory object (dynamic
// shared memory). It is freed once it has been removed (or unlinked) and
// nothing maps it or holds a descriptor for it.
type segment struct {
	id       int32
	name     string        // "" for anonymous segments
	seg      *vmem.Segment // nil until a named object is sized
	attached int
	fds      int
	removed  bool
}

// attachment is a segment mapped into a process.
type attachment struct {
	addr, len uint64
	s         *segment
}

type shmTable struct {
	mu    sync.Mutex
	segs  map[int32]*segment
	names map[string]*segment
	next  int32
}

func (t *shmTable) init() {
	t.segs = map[int32]*segment{}
	t.names = map[string]*segment{}
	t.next = 1
}

func (t *shmTable) newSegment(name string) *segment {
	s := &segment{id: t.next, name: name}
	t.next++
	t.segs[s.id] = s
	return s
}

func (t *shmTable) create(size uint32) int32 {
	if size == 0 {
		return -abi.EINVAL
	}
	seg, err := vmem.NewSegment(uint64(size))
	if err != nil {
		return -abi.ENOMEM
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.newSegment("")
	s.seg = seg
	return s.id
}

func (t *shmTable) size(id int32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil || s.removed || s.seg == nil {
		return -abi.ENOENT
	}
	return int32(s.seg.Size())
}

func (t *shmTable) attach(p *Process, id int32, addr uint32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil || s.removed || s.seg == nil {
		return -abi.ENOENT
	}
	return t.mapLocked(p, s, uint64(addr), s.seg.Size())
}

func (t *shmTable) mapLocked(p *Process, s *segment, addr, length uint64) int32 {
	if err := p.mem.MapRange(addr, s.seg, length); err != nil {
		p.k.debugf("[%d] shm map %d at %#x: %v", p.pid, s.id, addr, err)
		return -abi.EINVAL
	}
	// A new mapping over an old one replaces it.
	t.dropAttachmentsLocked(p, addr, length)
	s.attached++
	p.shmAttached = append(p.shmAttached, attachment{addr: addr, len: length, s: s})
	return 0
}

// dropAttachmentsLocked forgets p's attachments within [addr, addr+length).
func (t *shmTable) dropAttachmentsLocked(p *Process, addr, length uint64) {
	kept := p.shmAttached[:0]
	for _, a := range p.shmAttached {
		if a.addr >= addr && a.addr+a.len <= addr+length {
			a.s.attached--
			t.maybeFree(a.s)
			continue
		}
		kept = append(kept, a)
	}
	p.shmAttached = kept
}

// remove marks the segment for deletion once nothing uses it.
func (t *shmTable) remove(id int32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.segs[id]
	if s == nil || s.removed {
		return -abi.ENOENT
	}
	s.removed = true
	t.maybeFree(s)
	return 0
}

// open implements shm_open: it returns a host fd for the named object.
func (t *shmTable) open(p *Process, name string, flags int32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.names[name]
	switch {
	case s != nil && flags&abi.O_CREAT != 0 && flags&abi.O_EXCL != 0:
		return -abi.EEXIST
	case s == nil && flags&abi.O_CREAT == 0:
		return -abi.ENOENT
	case s == nil:
		s = t.newSegment(name)
		t.names[name] = s
	}
	s.fds++
	return p.fds.add(newOpenFile(&shmFile{t: t, s: s}, 0), false)
}

func (t *shmTable) unlink(name string) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.names[name]
	if s == nil {
		return -abi.ENOENT
	}
	delete(t.names, name)
	s.removed = true
	t.maybeFree(s)
	return 0
}

func (t *shmTable) truncate(s *segment, size uint32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.seg != nil {
		if uint64(size) <= s.seg.Size() {
			return 0
		}
		return -abi.EINVAL // resizing a sized object is not supported
	}
	if size == 0 {
		return 0
	}
	seg, err := vmem.NewSegment(uint64(size))
	if err != nil {
		return -abi.ENOMEM
	}
	s.seg = seg
	return 0
}

func (t *shmTable) fdSize(s *segment) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.seg == nil {
		return 0
	}
	return int32(s.seg.Size())
}

func (t *shmTable) mapFd(p *Process, s *segment, addr, length uint32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.seg == nil {
		return -abi.EINVAL
	}
	return t.mapLocked(p, s, uint64(addr), uint64(length))
}

func (t *shmTable) unmap(p *Process, addr, length uint32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := p.mem.Unmap(uint64(addr), uint64(length)); err != nil {
		return -abi.EINVAL
	}
	t.dropAttachmentsLocked(p, uint64(addr), uint64(length))
	return 0
}

// detachAll drops a finished process's attachments.
func (t *shmTable) detachAll(p *Process) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range p.shmAttached {
		a.s.attached--
		t.maybeFree(a.s)
	}
	p.shmAttached = nil
}

func (t *shmTable) maybeFree(s *segment) {
	if s.removed && s.attached == 0 && s.fds == 0 {
		delete(t.segs, s.id)
		if s.seg != nil {
			s.seg.Close()
			s.seg = nil
		}
	}
}

func (t *shmTable) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.segs {
		if s.seg != nil {
			s.seg.Close()
		}
		delete(t.segs, id)
	}
	t.names = map[string]*segment{}
}

// shmFile is a descriptor for a POSIX shared memory object.
type shmFile struct {
	t *shmTable
	s *segment
}

func (f *shmFile) read([]byte) (int, int32)  { return 0, abi.EINVAL }
func (f *shmFile) write([]byte) (int, int32) { return 0, abi.EINVAL }
func (f *shmFile) poll() int16               { return abi.POLLIN | abi.POLLOUT }
func (f *shmFile) changed() <-chan struct{}  { return nil }

func (f *shmFile) release() {
	f.t.mu.Lock()
	defer f.t.mu.Unlock()
	f.s.fds--
	f.t.maybeFree(f.s)
}
