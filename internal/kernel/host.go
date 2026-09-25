package kernel

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
)

// One host type per pgzero:host interface, each bound to the calling
// process. Results follow the WIT convention: >= 0, or a negated errno.
// A non-nil error unwinds the guest (exit, or SIGKILL during a blocking
// call).
type (
	procHost struct{ p *Process }
	sigHost  struct{ p *Process }
	fdHost   struct{ p *Process }
	netHost  struct{ p *Process }
	shmHost  struct{ p *Process }
	semaHost struct{ p *Process }
)

// ---- guest memory helpers ----

func (p *Process) readU32(addr uint32) (uint32, bool) {
	b, ok := p.mem.Slice(addr, 4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

func (p *Process) writeU32(addr, v uint32) bool {
	b, ok := p.mem.Slice(addr, 4)
	if !ok {
		return false
	}
	binary.LittleEndian.PutUint32(b, v)
	return true
}

// cstring reads a NUL-terminated string.
func (p *Process) cstring(addr uint32) (string, bool) {
	const maxLen = 1 << 16
	for n := uint32(0); n < maxLen; n++ {
		b, ok := p.mem.Slice(addr+n, 1)
		if !ok {
			return "", false
		}
		if b[0] == 0 {
			s, _ := p.mem.Slice(addr, n)
			return string(s), true
		}
	}
	return "", false
}

// writeSockaddr stores a into (addr, *lenAddr) with accept(2) semantics:
// truncated to the buffer, with the full length reported back.
func (p *Process) writeSockaddr(a *sockaddr, addr, lenAddr uint32) int32 {
	if addr == 0 || lenAddr == 0 {
		return 0
	}
	room, ok := p.readU32(lenAddr)
	if !ok {
		return -abi.EFAULT
	}
	enc := a.encode()
	n := min(uint32(len(enc)), room)
	b, ok := p.mem.Slice(addr, n)
	if !ok {
		return -abi.EFAULT
	}
	copy(b, enc)
	p.writeU32(lenAddr, uint32(len(enc)))
	return 0
}

// ---- proc ----

func (h procHost) Getpid(context.Context) (int32, error) { return h.p.pid, nil }

func (h procHost) Getppid(context.Context) (int32, error) {
	if h.p.parent == nil {
		return 1, nil
	}
	return h.p.parent.pid, nil
}

// strings reads a NULL-terminated char*[].
func (p *Process) strings(addr uint32) ([]string, bool) {
	var list []string
	for i := uint32(0); ; i++ {
		ptr, ok := p.readU32(addr + 4*i)
		if !ok {
			return nil, false
		}
		if ptr == 0 {
			return list, true
		}
		s, ok := p.cstring(ptr)
		if !ok {
			return nil, false
		}
		list = append(list, s)
	}
}

func (h procHost) Spawn(_ context.Context, argvAddr, envpAddr, cwdAddr uint32) (int32, error) {
	argv, ok := h.p.strings(argvAddr)
	if !ok {
		return -abi.EFAULT, nil
	}
	cwd, ok := h.p.cstring(cwdAddr)
	if !ok {
		return -abi.EFAULT, nil
	}
	var env [][2]string
	if envpAddr != 0 {
		vars, ok := h.p.strings(envpAddr)
		if !ok {
			return -abi.EFAULT, nil
		}
		for _, v := range vars {
			name, value, _ := strings.Cut(v, "=")
			env = append(env, [2]string{name, value})
		}
	}
	if len(argv) == 0 {
		return -abi.EINVAL, nil
	}
	if !h.p.k.rt.HasProgram(argv[0]) {
		return -abi.ENOENT, nil
	}
	return h.p.spawn(argv, env, cwd), nil
}

func (h procHost) Waitpid(_ context.Context, pid int32, statusAddr uint32, options int32) (int32, error) {
	cpid, status, errno := h.p.waitpid(pid, options)
	if errno < 0 {
		return errno, nil
	}
	if cpid > 0 && statusAddr != 0 && !h.p.writeU32(statusAddr, uint32(status)) {
		return -abi.EFAULT, nil
	}
	return cpid, nil
}

func (h procHost) Kill(_ context.Context, pid, sig int32) (int32, error) {
	return h.p.kill(pid, sig), nil
}

func (h procHost) Exit(_ context.Context, status int32) error {
	return h.p.exit(status)
}

// ---- sig ----

func (h sigHost) Register(_ context.Context, addr uint32) (int32, error) {
	return h.p.registerSignals(addr), nil
}

func (h sigHost) SetAlarm(_ context.Context, valueUsec, intervalUsec uint64) (int64, error) {
	rem := h.p.setAlarm(time.Duration(valueUsec)*time.Microsecond,
		time.Duration(intervalUsec)*time.Microsecond)
	return rem.Microseconds(), nil
}

func (h sigHost) Sleep(_ context.Context, usec uint64) (int32, error) {
	var deadline time.Time
	if usec != math.MaxUint64 {
		deadline = time.Now().Add(time.Duration(usec) * time.Microsecond)
	}
	switch h.p.block(deadline) {
	case interrupted:
		return -abi.EINTR, nil
	case killed:
		return 0, errKilled
	}
	return 0, nil
}

// ---- fd ----

func (h fdHost) Pipe(_ context.Context, fdsAddr uint32, flags int32) (int32, error) {
	r, w := newPipe()
	nb := flags & abi.O_NONBLOCK
	rfd := h.p.fds.add(newOpenFile(r, nb), false)
	wfd := h.p.fds.add(newOpenFile(w, nb), false)
	if !h.p.writeU32(fdsAddr, uint32(rfd)) || !h.p.writeU32(fdsAddr+4, uint32(wfd)) {
		h.p.fds.close(rfd)
		h.p.fds.close(wfd)
		return -abi.EFAULT, nil
	}
	return 0, nil
}

func (h fdHost) Devnull(context.Context) (int32, error) {
	return h.p.fds.add(newOpenFile(nullFile{}, 0), false), nil
}

func (h fdHost) Read(_ context.Context, fd int32, buf, n uint32) (int32, error) {
	of, ok := h.p.fds.get(fd)
	if !ok {
		return -abi.EBADF, nil
	}
	b, ok := h.p.mem.Slice(buf, n)
	if !ok {
		return -abi.EFAULT, nil
	}
	return h.p.io(of, func() (int, int32) { return of.f.read(b) })
}

func (h fdHost) Write(_ context.Context, fd int32, buf, n uint32) (int32, error) {
	of, ok := h.p.fds.get(fd)
	if !ok {
		return -abi.EBADF, nil
	}
	b, ok := h.p.mem.Slice(buf, n)
	if !ok {
		return -abi.EFAULT, nil
	}
	r, err := h.p.io(of, func() (int, int32) { return of.f.write(b) })
	if r == -abi.EPIPE {
		h.p.Signal(abi.SIGPIPE)
	}
	return r, err
}

func (h fdHost) Close(_ context.Context, fd int32) (int32, error) {
	return h.p.fds.close(fd), nil
}

func validHostFD(fd int32) bool {
	return (fd >= 0 && fd <= 2) || fd >= abi.FDBase
}

func (h fdHost) Dup(_ context.Context, fd, newFd int32) (int32, error) {
	of, ok := h.p.fds.get(fd)
	if !ok {
		return -abi.EBADF, nil
	}
	if newFd < 0 {
		return h.p.fds.add(of, false), nil
	}
	if !validHostFD(newFd) {
		return -abi.EBADF, nil
	}
	if newFd != fd {
		h.p.fds.install(newFd, of, false)
	}
	return newFd, nil
}

func (h fdHost) Fcntl(_ context.Context, fd, cmd, arg int32) (int32, error) {
	of, ok := h.p.fds.get(fd)
	if !ok {
		return -abi.EBADF, nil
	}
	switch cmd {
	case abi.F_GETFD:
		cloexec, _ := h.p.fds.getCloexec(fd)
		if cloexec {
			return abi.FD_CLOEXEC, nil
		}
		return 0, nil
	case abi.F_SETFD:
		h.p.fds.setCloexec(fd, arg&abi.FD_CLOEXEC != 0)
		return 0, nil
	case abi.F_GETFL:
		return of.flags.Load(), nil
	case abi.F_SETFL:
		of.flags.Store(arg & abi.O_NONBLOCK)
		return 0, nil
	case abi.F_DUPFD, abi.F_DUPFD_CLOEXEC:
		return h.p.fds.add(of, cmd == abi.F_DUPFD_CLOEXEC), nil
	}
	return -abi.EINVAL, nil
}

func (h fdHost) Poll(_ context.Context, fdsAddr, nfds uint32, timeoutMs int32) (int32, error) {
	return h.p.poll(fdsAddr, nfds, timeoutMs)
}

// ---- net ----

func (p *Process) socket(fd int32) (*socket, *openFile, int32) {
	of, ok := p.fds.get(fd)
	if !ok {
		return nil, nil, -abi.EBADF
	}
	s, ok := of.f.(*socket)
	if !ok {
		return nil, nil, -abi.ENOTSOCK
	}
	return s, of, 0
}

func (p *Process) sockaddrArg(addr, n uint32) (*sockaddr, int32) {
	b, ok := p.mem.Slice(addr, n)
	if !ok {
		return nil, -abi.EFAULT
	}
	a, errno := decodeSockaddr(b)
	if errno != 0 {
		return nil, -errno
	}
	return a, 0
}

func (h netHost) Socket(_ context.Context, domain, socktype, protocol int32) (int32, error) {
	switch domain {
	case abi.AF_INET, abi.AF_INET6, abi.AF_UNIX:
	default:
		return -abi.EAFNOSUPPORT, nil
	}
	if socktype&^(abi.SOCK_NONBLOCK|abi.SOCK_CLOEXEC) != abi.SOCK_STREAM {
		return -abi.EPROTONOSUPPORT, nil
	}
	var flags int32
	if socktype&abi.SOCK_NONBLOCK != 0 {
		flags = abi.O_NONBLOCK
	}
	s := newSocket(h.p.k, domain)
	return h.p.fds.add(newOpenFile(s, flags), socktype&abi.SOCK_CLOEXEC != 0), nil
}

func (h netHost) Bind(_ context.Context, fd int32, addr, n uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	a, errno := h.p.sockaddrArg(addr, n)
	if errno != 0 {
		return errno, nil
	}
	return -s.bind(a), nil
}

func (h netHost) Listen(_ context.Context, fd, backlog int32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	return -s.listen(), nil
}

func (h netHost) Accept(_ context.Context, fd int32, addr, lenAddr uint32, flags int32) (int32, error) {
	s, of, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	var ns *socket
	r, err := h.p.io(of, func() (int, int32) {
		var e int32
		ns, e = s.accept()
		return 0, e
	})
	if r < 0 || err != nil {
		return r, err
	}
	var nflags int32
	if flags&abi.SOCK_NONBLOCK != 0 {
		nflags = abi.O_NONBLOCK
	}
	nfd := h.p.fds.add(newOpenFile(ns, nflags), flags&abi.SOCK_CLOEXEC != 0)
	if ns.peer != nil {
		if e := h.p.writeSockaddr(ns.peer, addr, lenAddr); e != 0 {
			h.p.fds.close(nfd)
			return e, nil
		}
	}
	return nfd, nil
}

func (h netHost) Connect(_ context.Context, fd int32, addr, n uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	a, errno := h.p.sockaddrArg(addr, n)
	if errno != 0 {
		return errno, nil
	}
	return -s.connect(h.p.ctx, a), nil
}

func (h netHost) Recv(_ context.Context, fd int32, buf, n uint32, flags int32) (int32, error) {
	s, of, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	b, ok := h.p.mem.Slice(buf, n)
	if !ok {
		return -abi.EFAULT, nil
	}
	peek := flags&abi.MSG_PEEK != 0
	if flags&abi.MSG_DONTWAIT != 0 {
		r, e := s.recv(b, peek)
		if e != 0 {
			return -e, nil
		}
		return int32(r), nil
	}
	return h.p.io(of, func() (int, int32) { return s.recv(b, peek) })
}

func (h netHost) Send(_ context.Context, fd int32, buf, n uint32, flags int32) (int32, error) {
	s, of, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	b, ok := h.p.mem.Slice(buf, n)
	if !ok {
		return -abi.EFAULT, nil
	}
	var r int32
	var err error
	if flags&abi.MSG_DONTWAIT != 0 {
		w, e := s.send(b)
		r = int32(w)
		if e != 0 {
			r = -e
		}
	} else {
		r, err = h.p.io(of, func() (int, int32) { return s.send(b) })
	}
	if r == -abi.EPIPE && flags&abi.MSG_NOSIGNAL == 0 {
		h.p.Signal(abi.SIGPIPE)
	}
	return r, err
}

func (h netHost) Shutdown(_ context.Context, fd, how int32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	return -s.shutdown(how), nil
}

func (h netHost) Getsockopt(_ context.Context, fd, level, name int32, val, lenAddr uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	room, ok := h.p.readU32(lenAddr)
	if !ok {
		return -abi.EFAULT, nil
	}
	if room < 4 {
		return -abi.EINVAL, nil
	}
	if !h.p.writeU32(val, uint32(s.getsockopt(level, name))) || !h.p.writeU32(lenAddr, 4) {
		return -abi.EFAULT, nil
	}
	return 0, nil
}

func (h netHost) Setsockopt(_ context.Context, fd, level, name int32, val, n uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	var v uint32
	if n >= 4 {
		var ok bool
		if v, ok = h.p.readU32(val); !ok {
			return -abi.EFAULT, nil
		}
	}
	s.setsockopt(level, name, int32(v))
	return 0, nil
}

func (h netHost) Getsockname(_ context.Context, fd int32, addr, lenAddr uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	return h.p.writeSockaddr(s.getsockname(), addr, lenAddr), nil
}

func (h netHost) Getpeername(_ context.Context, fd int32, addr, lenAddr uint32) (int32, error) {
	s, _, errno := h.p.socket(fd)
	if errno != 0 {
		return errno, nil
	}
	a, e := s.getpeername()
	if e != 0 {
		return -e, nil
	}
	return h.p.writeSockaddr(a, addr, lenAddr), nil
}

// ---- shm ----

func (h shmHost) Create(_ context.Context, size uint32) (int32, error) {
	return h.p.k.shm.create(size), nil
}

func (h shmHost) Size(_ context.Context, id int32) (int32, error) {
	return h.p.k.shm.size(id), nil
}

func (h shmHost) Attach(_ context.Context, id int32, addr uint32) (int32, error) {
	return h.p.k.shm.attach(h.p, id, addr), nil
}

func (h shmHost) Remove(_ context.Context, id int32) (int32, error) {
	return h.p.k.shm.remove(id), nil
}

func (p *Process) shmFD(fd int32) (*shmFile, int32) {
	of, ok := p.fds.get(fd)
	if !ok {
		return nil, -abi.EBADF
	}
	f, ok := of.f.(*shmFile)
	if !ok {
		return nil, -abi.EINVAL
	}
	return f, 0
}

func (h shmHost) Open(_ context.Context, nameAddr, nameLen uint32, flags int32) (int32, error) {
	b, ok := h.p.mem.Slice(nameAddr, nameLen)
	if !ok {
		return -abi.EFAULT, nil
	}
	return h.p.k.shm.open(h.p, string(b), flags), nil
}

func (h shmHost) Unlink(_ context.Context, nameAddr, nameLen uint32) (int32, error) {
	b, ok := h.p.mem.Slice(nameAddr, nameLen)
	if !ok {
		return -abi.EFAULT, nil
	}
	return h.p.k.shm.unlink(string(b)), nil
}

func (h shmHost) Truncate(_ context.Context, fd int32, size uint32) (int32, error) {
	f, errno := h.p.shmFD(fd)
	if errno != 0 {
		return errno, nil
	}
	return h.p.k.shm.truncate(f.s, size), nil
}

func (h shmHost) FdSize(_ context.Context, fd int32) (int32, error) {
	f, errno := h.p.shmFD(fd)
	if errno != 0 {
		return errno, nil
	}
	return h.p.k.shm.fdSize(f.s), nil
}

func (h shmHost) MapFd(_ context.Context, fd int32, addr, n uint32) (int32, error) {
	f, errno := h.p.shmFD(fd)
	if errno != 0 {
		return errno, nil
	}
	return h.p.k.shm.mapFd(h.p, f.s, addr, n), nil
}

func (h shmHost) Unmap(_ context.Context, addr, n uint32) (int32, error) {
	return h.p.k.shm.unmap(h.p, addr, n), nil
}

// ---- sema ----

func (h semaHost) Create(_ context.Context, initial int32) (int32, error) {
	return h.p.k.sems.create(initial), nil
}

func (h semaHost) Lock(_ context.Context, id int32) (int32, error) {
	s := h.p.k.sems.get(id)
	if s == nil {
		return -abi.EINVAL, nil
	}
	return h.p.semaLock(s)
}

func (h semaHost) TryLock(_ context.Context, id int32) (int32, error) {
	s := h.p.k.sems.get(id)
	if s == nil {
		return -abi.EINVAL, nil
	}
	if s.tryLock() {
		return 0, nil
	}
	return -abi.EAGAIN, nil
}

func (h semaHost) Unlock(_ context.Context, id int32) (int32, error) {
	s := h.p.k.sems.get(id)
	if s == nil {
		return -abi.EINVAL, nil
	}
	s.unlock()
	return 0, nil
}

func (h semaHost) Reset(_ context.Context, id int32) (int32, error) {
	s := h.p.k.sems.get(id)
	if s == nil {
		return -abi.EINVAL, nil
	}
	s.reset()
	return 0, nil
}
