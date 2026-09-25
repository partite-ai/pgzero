package kernel

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
)

const (
	sockReadAhead = 256 << 10
	sockWriteBuf  = 256 << 10
)

type sockState int

const (
	sockNew sockState = iota
	sockListening
	sockConnected
)

// sockaddr is a decoded guest socket address.
type sockaddr struct {
	family int32
	ap     netip.AddrPort // AF_INET, AF_INET6
	path   string         // AF_UNIX guest path
}

// socket is a stream socket backed by the host network.
type socket struct {
	k      *Kernel
	domain int32

	mu    sync.Mutex
	n     notifier
	state sockState
	bound *sockaddr
	peer  *sockaddr
	opts  map[[2]int32]int32

	// listening: a Unix socket has a virtual listener (see
	// Kernel.DialUnix) and possibly a real one as well.
	lns     []net.Listener
	acceptQ []net.Conn

	// connected
	conn     net.Conn
	rbuf     []byte
	rerr     error // io.EOF or a connection error, once the reader stops
	rcond    *sync.Cond
	wbuf     []byte
	werr     error
	wcond    *sync.Cond
	wshut    bool // shutdown(SHUT_WR) or closing: stop after draining
	wdone    bool // writeLoop has finished
	released bool
}

func newSocket(k *Kernel, domain int32) *socket {
	s := &socket{k: k, domain: domain, opts: map[[2]int32]int32{}}
	s.rcond = sync.NewCond(&s.mu)
	s.wcond = sync.NewCond(&s.mu)
	return s
}

// decodeSockaddr parses a guest sockaddr.
func decodeSockaddr(b []byte) (*sockaddr, int32) {
	if len(b) < 2 {
		return nil, abi.EINVAL
	}
	family := int32(binary.LittleEndian.Uint16(b))
	switch family {
	case abi.AF_INET:
		if len(b) < abi.SizeofSockaddrIn {
			return nil, abi.EINVAL
		}
		port := binary.BigEndian.Uint16(b[2:])
		addr := netip.AddrFrom4([4]byte(b[4:8]))
		return &sockaddr{family: family, ap: netip.AddrPortFrom(addr, port)}, 0
	case abi.AF_INET6:
		if len(b) < 28 {
			return nil, abi.EINVAL
		}
		port := binary.BigEndian.Uint16(b[2:])
		addr := netip.AddrFrom16([16]byte(b[8:24]))
		return &sockaddr{family: family, ap: netip.AddrPortFrom(addr, port)}, 0
	case abi.AF_UNIX:
		p := b[2:]
		for i, c := range p {
			if c == 0 {
				p = p[:i]
				break
			}
		}
		if len(p) == 0 {
			return nil, abi.EINVAL
		}
		return &sockaddr{family: family, path: string(p)}, 0
	}
	return nil, abi.EAFNOSUPPORT
}

// encode renders the address in guest layout.
func (a *sockaddr) encode() []byte {
	switch a.family {
	case abi.AF_INET:
		b := make([]byte, abi.SizeofSockaddrIn)
		binary.LittleEndian.PutUint16(b, abi.AF_INET)
		binary.BigEndian.PutUint16(b[2:], a.ap.Port())
		ip := a.ap.Addr().Unmap().As4()
		copy(b[4:], ip[:])
		return b
	case abi.AF_INET6:
		b := make([]byte, abi.SizeofSockaddrIn6)
		binary.LittleEndian.PutUint16(b, abi.AF_INET6)
		binary.BigEndian.PutUint16(b[2:], a.ap.Port())
		ip := a.ap.Addr().As16()
		copy(b[8:], ip[:])
		return b
	default:
		b := make([]byte, 2, 2+len(a.path)+1)
		binary.LittleEndian.PutUint16(b, abi.AF_UNIX)
		b = append(b, a.path...)
		return append(b, 0)
	}
}

func sockaddrFromNet(domain int32, addr net.Addr) *sockaddr {
	switch a := addr.(type) {
	case *net.TCPAddr:
		ap := a.AddrPort()
		if domain == abi.AF_INET {
			return &sockaddr{family: abi.AF_INET, ap: netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
		}
		return &sockaddr{family: abi.AF_INET6, ap: ap}
	}
	// Unix peers are unnamed.
	return &sockaddr{family: abi.AF_UNIX}
}

func netErrno(err error) int32 {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EADDRINUSE:
			return abi.EADDRINUSE
		case syscall.EADDRNOTAVAIL:
			return abi.EADDRNOTAVAIL
		case syscall.ECONNREFUSED:
			return abi.ECONNREFUSED
		case syscall.ECONNRESET:
			return abi.ECONNRESET
		case syscall.EACCES, syscall.EPERM:
			return abi.EACCES
		case syscall.ENOENT:
			return abi.ENOENT
		case syscall.ENETUNREACH:
			return abi.ENETUNREACH
		case syscall.EHOSTUNREACH:
			return abi.EHOSTUNREACH
		case syscall.ETIMEDOUT:
			return abi.ETIMEDOUT
		case syscall.EPIPE:
			return abi.EPIPE
		case syscall.EINVAL:
			return abi.EINVAL
		}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return abi.ETIMEDOUT
	}
	return abi.EIO
}

// bind creates the host listener right away (Go has no separate bind),
// so that the address is claimed - and a Unix socket file exists, which
// Postgres chmods before calling listen - as with a real bind(2).
// Connections are only accepted once listen is called.
func (s *socket) bind(a *sockaddr) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.family != s.domain {
		return abi.EAFNOSUPPORT
	}
	if s.bound != nil || s.state != sockNew {
		return abi.EINVAL
	}
	var lns []net.Listener
	var err error
	switch s.domain {
	case abi.AF_INET:
		var ln net.Listener
		ln, err = net.Listen("tcp4", a.ap.String())
		lns = append(lns, ln)
	case abi.AF_INET6:
		// "tcp6" listens IPv6-only, as Postgres asks with IPV6_V6ONLY.
		var ln net.Listener
		ln, err = net.Listen("tcp6", a.ap.String())
		lns = append(lns, ln)
	case abi.AF_UNIX:
		var vl *virtualListener
		if vl, err = s.k.listenUnix(a.path); err != nil {
			return netErrno(err)
		}
		lns = append(lns, vl)
		// Like a real bind, make the socket visible in the filesystem.
		s.k.createSocketFile(a.path)
		if hp, ok := s.k.hostPath(a.path); ok {
			ln, err := net.Listen("unix", hp)
			if err != nil {
				vl.Close()
				return netErrno(err)
			}
			lns = append(lns, ln)
		}
	}
	if err != nil {
		return netErrno(err)
	}
	if s.domain == abi.AF_UNIX {
		s.bound = a
	} else {
		// Record the actual port (in case it was 0).
		s.bound = sockaddrFromNet(s.domain, lns[0].Addr())
	}
	s.lns = lns
	return 0
}

func (s *socket) listen() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == sockListening {
		return 0
	}
	if s.state != sockNew || len(s.lns) == 0 {
		return abi.EINVAL
	}
	s.state = sockListening
	for _, ln := range s.lns {
		go s.acceptLoop(ln)
	}
	return 0
}

func (s *socket) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.released {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.acceptQ = append(s.acceptQ, c)
		s.mu.Unlock()
		s.n.notify()
	}
}

// accept takes a pending connection, or returns EAGAIN.
func (s *socket) accept() (*socket, int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sockListening {
		return nil, abi.EINVAL
	}
	if len(s.acceptQ) == 0 {
		return nil, abi.EAGAIN
	}
	c := s.acceptQ[0]
	s.acceptQ = s.acceptQ[1:]
	ns := newSocket(s.k, s.domain)
	ns.bound = s.bound
	ns.peer = sockaddrFromNet(s.domain, c.RemoteAddr())
	ns.startConn(c)
	return ns, 0
}

func (s *socket) connect(ctx context.Context, a *sockaddr) int32 {
	s.mu.Lock()
	if s.state != sockNew {
		s.mu.Unlock()
		return abi.EISCONN
	}
	s.mu.Unlock()
	if a.family != s.domain {
		return abi.EAFNOSUPPORT
	}
	var d net.Dialer
	var c net.Conn
	var err error
	switch a.family {
	case abi.AF_UNIX:
		if c, err = s.k.DialUnix(ctx, a.path); err != nil {
			hp, ok := s.k.hostPath(a.path)
			if !ok {
				return abi.ECONNREFUSED
			}
			c, err = d.DialContext(ctx, "unix", hp)
		}
	default:
		c, err = d.DialContext(ctx, "tcp", a.ap.String())
	}
	if err != nil {
		return netErrno(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peer = a
	if s.bound == nil {
		s.bound = sockaddrFromNet(s.domain, c.LocalAddr())
	}
	s.startConn(c)
	return 0
}

// startConn begins pumping c. Called with s.mu held.
func (s *socket) startConn(c net.Conn) {
	s.conn = c
	s.state = sockConnected
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	go s.readLoop(c)
	go s.writeLoop(c)
}

func (s *socket) readLoop(c net.Conn) {
	chunk := make([]byte, 64<<10)
	for {
		n, err := c.Read(chunk)
		s.mu.Lock()
		if n > 0 {
			s.rbuf = append(s.rbuf, chunk[:n]...)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.rerr = err
			} else {
				s.rerr = io.EOF
			}
		}
		for len(s.rbuf) >= sockReadAhead && s.rerr == nil && !s.released {
			s.rcond.Wait()
		}
		stop := s.rerr != nil || s.released
		s.mu.Unlock()
		s.n.notify()
		if stop {
			return
		}
	}
}

func (s *socket) writeLoop(c net.Conn) {
	s.mu.Lock()
	for {
		for len(s.wbuf) == 0 && !s.wshut && s.werr == nil {
			s.wcond.Wait()
		}
		if s.werr != nil {
			break
		}
		if len(s.wbuf) == 0 && s.wshut {
			break
		}
		out := s.wbuf
		s.mu.Unlock()
		n, err := c.Write(out)
		s.mu.Lock()
		s.wbuf = s.wbuf[n:]
		if len(s.wbuf) == 0 {
			s.wbuf = nil
		}
		if err != nil {
			s.werr = err
		}
		s.n.notify()
	}
	released := s.released
	s.wdone = true
	s.mu.Unlock()
	if released {
		c.Close()
	} else if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	s.n.notify()
}

func (s *socket) recv(b []byte, peek bool) (int, int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sockConnected {
		return 0, abi.ENOTCONN
	}
	if len(b) == 0 {
		return 0, 0
	}
	if len(s.rbuf) > 0 {
		n := copy(b, s.rbuf)
		if !peek {
			s.rbuf = s.rbuf[n:]
			if len(s.rbuf) == 0 {
				s.rbuf = nil
			}
			s.rcond.Signal()
		}
		return n, 0
	}
	switch {
	case s.rerr == io.EOF:
		return 0, 0
	case s.rerr != nil:
		return 0, abi.ECONNRESET
	}
	return 0, abi.EAGAIN
}

func (s *socket) send(b []byte) (int, int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sockConnected {
		return 0, abi.ENOTCONN
	}
	if s.werr != nil || s.wshut {
		return 0, abi.EPIPE
	}
	if len(b) == 0 {
		return 0, 0
	}
	space := sockWriteBuf - len(s.wbuf)
	if space <= 0 {
		return 0, abi.EAGAIN
	}
	n := min(len(b), space)
	s.wbuf = append(s.wbuf, b[:n]...)
	s.wcond.Signal()
	return n, 0
}

func (s *socket) read(b []byte) (int, int32)  { return s.recv(b, false) }
func (s *socket) write(b []byte) (int, int32) { return s.send(b) }

func (s *socket) poll() int16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ev int16
	switch s.state {
	case sockListening:
		if len(s.acceptQ) > 0 {
			ev |= abi.POLLIN
		}
	case sockConnected:
		if len(s.rbuf) > 0 || s.rerr != nil {
			ev |= abi.POLLIN
		}
		if s.rerr != nil && s.rerr != io.EOF {
			ev |= abi.POLLERR
		}
		if s.werr != nil {
			ev |= abi.POLLERR
		} else if len(s.wbuf) < sockWriteBuf && !s.wshut {
			ev |= abi.POLLOUT
		}
		if s.rerr != nil && (s.wshut || s.werr != nil) {
			ev |= abi.POLLHUP
		}
	}
	return ev
}

func (s *socket) changed() <-chan struct{} { return s.n.wait() }

func (s *socket) shutdown(how int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sockConnected {
		return abi.ENOTCONN
	}
	if how == abi.SHUT_WR || how == abi.SHUT_RDWR {
		s.wshut = true
		s.wcond.Signal()
	}
	return 0
}

// release closes the socket once the last descriptor is gone. Buffered
// output is still flushed, briefly, as the kernel would.
func (s *socket) release() {
	s.mu.Lock()
	s.released = true
	lns, conn, queued, wdone := s.lns, s.conn, s.acceptQ, s.wdone
	s.acceptQ = nil
	s.wshut = true
	s.wcond.Signal()
	s.rcond.Signal()
	s.mu.Unlock()
	for _, ln := range lns {
		ln.Close()
	}
	for _, c := range queued {
		c.Close()
	}
	switch {
	case conn != nil && wdone:
		conn.Close()
	case conn != nil:
		// writeLoop closes conn after draining; don't wait forever for a
		// peer that stopped reading.
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	s.n.notify()
}

func (s *socket) getsockname() *sockaddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bound != nil {
		return s.bound
	}
	switch s.domain {
	case abi.AF_INET:
		return &sockaddr{family: abi.AF_INET, ap: netip.AddrPortFrom(netip.IPv4Unspecified(), 0)}
	case abi.AF_INET6:
		return &sockaddr{family: abi.AF_INET6, ap: netip.AddrPortFrom(netip.IPv6Unspecified(), 0)}
	}
	return &sockaddr{family: abi.AF_UNIX}
}

func (s *socket) getpeername() (*sockaddr, int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sockConnected || s.peer == nil {
		return nil, abi.ENOTCONN
	}
	return s.peer, 0
}

func (s *socket) getsockopt(level, name int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if level == abi.SOL_SOCKET {
		switch name {
		case abi.SO_ERROR:
			return 0
		case abi.SO_TYPE:
			return abi.SOCK_STREAM
		case abi.SO_RCVBUF:
			if v, ok := s.opts[[2]int32{level, name}]; ok {
				return v
			}
			return sockReadAhead
		case abi.SO_SNDBUF:
			if v, ok := s.opts[[2]int32{level, name}]; ok {
				return v
			}
			return sockWriteBuf
		}
	}
	return s.opts[[2]int32{level, name}]
}

func (s *socket) setsockopt(level, name, value int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts[[2]int32{level, name}] = value
	tc, ok := s.conn.(*net.TCPConn)
	if !ok {
		return
	}
	switch {
	case level == abi.IPPROTO_TCP && name == abi.TCP_NODELAY:
		tc.SetNoDelay(value != 0)
	case level == abi.SOL_SOCKET && name == abi.SO_KEEPALIVE:
		tc.SetKeepAlive(value != 0)
	}
}
