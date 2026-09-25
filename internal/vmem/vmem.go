// Package vmem provides mmap-backed wasm linear memories whose address
// ranges can be overlaid with shared segments.
//
// Each simulated process gets its own Memory: a private anonymous mapping
// that reserves the module's full maximum size up front, so the backing
// buffer never moves when the guest grows its memory. A Segment (a shared,
// fd-backed mapping) can then be mapped over a fixed range of any number of
// Memories, which gives every process a genuinely shared region at the same
// guest address - the same arrangement Postgres gets from SysV/POSIX shared
// memory mapped at a fixed address in each backend.
package vmem

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/sys/unix"
)

// PageSize is the wasm page size. Every range handed to mmap/mprotect is a
// multiple of it, which is also a multiple of the host page size (4K/16K).
const PageSize = 65536

// Memory is an experimental.LinearMemory backed by a fixed reservation.
type Memory struct {
	mu        sync.Mutex
	buf       []byte        // the whole reservation; len == reserved
	committed uint64        // bytes currently accessible (page aligned)
	size      atomic.Uint64 // the guest-visible memory size
}

var _ experimental.LinearMemory = (*Memory)(nil)

// Allocator returns an experimental.MemoryAllocator that creates a new
// Memory and reports it to onAlloc, so the caller can associate the
// memory with the process being instantiated.
func Allocator(onAlloc func(*Memory)) experimental.MemoryAllocator {
	return experimental.MemoryAllocatorFunc(func(cap, max uint64) experimental.LinearMemory {
		m, err := Reserve(max)
		if err != nil {
			// wazero has no error path here; a nil buffer from Reallocate
			// makes instantiation fail instead.
			return failedMemory{}
		}
		if onAlloc != nil {
			onAlloc(m)
		}
		return m
	})
}

// Reserve reserves max bytes of address space with no access. Nothing is
// committed until Reallocate.
func Reserve(max uint64) (*Memory, error) {
	if max == 0 {
		max = PageSize
	}
	max = alignUp(max)
	p, err := unix.MmapPtr(-1, 0, nil, uintptr(max), unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("vmem: reserve %d bytes: %w", max, err)
	}
	return &Memory{buf: unsafe.Slice((*byte)(p), max)}, nil
}

// Reallocate implements experimental.LinearMemory. It only ever makes more
// of the reservation accessible; the base address never changes.
func (m *Memory) Reallocate(size uint64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > uint64(len(m.buf)) {
		return nil
	}
	if want := alignUp(size); want > m.committed {
		if err := unix.Mprotect(m.buf[m.committed:want], unix.PROT_READ|unix.PROT_WRITE); err != nil {
			return nil
		}
		m.committed = want
	}
	m.size.Store(size)
	return m.buf[:size]
}

// Size returns the guest-visible size of the memory in bytes.
func (m *Memory) Size() uint64 { return m.size.Load() }

// Slice returns the guest memory [addr, addr+n), or false if the range is
// outside the memory. The slice aliases guest memory. The backing buffer
// never moves, so it stays valid, and may be used from any goroutine, for
// as long as the memory is not freed.
func (m *Memory) Slice(addr, n uint32) ([]byte, bool) {
	end := uint64(addr) + uint64(n)
	if end > m.size.Load() {
		return nil, false
	}
	return m.buf[addr:end:end], true
}

// Uint64Ptr returns a pointer to the 8-byte aligned uint64 at addr, for
// atomic access shared with the guest.
func (m *Memory) Uint64Ptr(addr uint32) (*uint64, bool) {
	if addr%8 != 0 || uint64(addr)+8 > m.size.Load() {
		return nil, false
	}
	return (*uint64)(unsafe.Pointer(&m.buf[addr])), true
}

// Free implements experimental.LinearMemory.
func (m *Memory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buf != nil {
		_ = unix.MunmapPtr(unsafe.Pointer(&m.buf[0]), uintptr(len(m.buf)))
		m.buf = nil
		m.committed = 0
		m.size.Store(0)
	}
}

// Map overlays all of seg onto [addr, addr+seg.Size()) of this memory.
func (m *Memory) Map(addr uint64, seg *Segment) error {
	return m.MapRange(addr, seg, seg.size)
}

// MapRange overlays the first length bytes of seg onto [addr,
// addr+length). The range must be page aligned and already committed
// (the guest grows its memory, or allocates the range, first).
func (m *Memory) MapRange(addr uint64, seg *Segment, length uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if addr%PageSize != 0 || length%PageSize != 0 || length == 0 {
		return fmt.Errorf("vmem: map [%#x, +%#x) is not page aligned", addr, length)
	}
	if length > seg.size {
		return fmt.Errorf("vmem: map length %#x exceeds segment size %#x", length, seg.size)
	}
	end := addr + length
	if end > m.committed {
		return fmt.Errorf("vmem: map range [%#x, %#x) exceeds committed memory %#x", addr, end, m.committed)
	}
	_, err := unix.MmapPtr(int(seg.f.Fd()), 0, unsafe.Pointer(&m.buf[addr]), uintptr(length),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED)
	if err != nil {
		return fmt.Errorf("vmem: map segment at %#x: %w", addr, err)
	}
	return nil
}

// Unmap replaces [addr, addr+length) with private zeroed memory again.
func (m *Memory) Unmap(addr, length uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if addr%PageSize != 0 || length%PageSize != 0 {
		return fmt.Errorf("vmem: unmap [%#x, +%#x) is not page aligned", addr, length)
	}
	if addr+length > m.committed {
		return fmt.Errorf("vmem: unmap range exceeds committed memory")
	}
	_, err := unix.MmapPtr(-1, 0, unsafe.Pointer(&m.buf[addr]), uintptr(length),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_FIXED)
	return err
}

// Segment is a shared memory object that can be mapped into many Memories.
type Segment struct {
	f    *os.File
	size uint64
	view []byte // host-side mapping, for the host to inspect the segment
}

// NewSegment creates an anonymous shared segment of the given size.
//
// TODO: this uses an unlinked temp file; use memfd_create on Linux and
// shm_open on darwin so the pages are never written back to disk.
func NewSegment(size uint64) (*Segment, error) {
	size = alignUp(size)
	f, err := os.CreateTemp("", "pgzero-shm-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return nil, err
	}
	view, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Segment{f: f, size: size, view: view}, nil
}

// Size returns the segment size in bytes.
func (s *Segment) Size() uint64 { return s.size }

// Bytes returns the host's own view of the segment.
func (s *Segment) Bytes() []byte { return s.view }

// Close unmaps the host view and releases the segment. Mappings in guest
// memories stay valid until those memories are freed.
func (s *Segment) Close() error {
	_ = unix.Munmap(s.view)
	return s.f.Close()
}

func alignUp(n uint64) uint64 { return (n + PageSize - 1) &^ (PageSize - 1) }

type failedMemory struct{}

func (failedMemory) Reallocate(uint64) []byte { return nil }
func (failedMemory) Free()                    {}
