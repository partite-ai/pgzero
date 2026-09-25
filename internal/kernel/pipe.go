package kernel

import (
	"sync"

	"github.com/partite-ai/pgzero/internal/abi"
)

const (
	pipeCapacity = 64 << 10
	// pipeAtomic is PIPE_BUF: writes up to this size are never split.
	// Postgres's syslogger protocol depends on it.
	pipeAtomic = 4096
)

// pipe is an in-memory pipe shared by its read and write ends.
type pipe struct {
	mu      sync.Mutex
	buf     []byte
	readers int
	writers int
	n       notifier
}

func newPipe() (*pipeReader, *pipeWriter) {
	p := &pipe{readers: 1, writers: 1}
	return &pipeReader{p}, &pipeWriter{p}
}

type pipeReader struct{ p *pipe }

func (r *pipeReader) read(b []byte) (int, int32) {
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(b) == 0 {
		return 0, 0
	}
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		if len(p.buf) == 0 {
			p.buf = nil
		}
		p.n.notify()
		return n, 0
	}
	if p.writers == 0 {
		return 0, 0
	}
	return 0, abi.EAGAIN
}

func (r *pipeReader) write([]byte) (int, int32) { return 0, abi.EBADF }

func (r *pipeReader) poll() int16 {
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	var ev int16
	if len(p.buf) > 0 {
		ev |= abi.POLLIN
	}
	if p.writers == 0 {
		ev |= abi.POLLHUP
	}
	return ev
}

func (r *pipeReader) changed() <-chan struct{} { return r.p.n.wait() }

func (r *pipeReader) release() {
	p := r.p
	p.mu.Lock()
	p.readers--
	p.mu.Unlock()
	p.n.notify()
}

type pipeWriter struct{ p *pipe }

func (w *pipeWriter) read([]byte) (int, int32) { return 0, abi.EBADF }

func (w *pipeWriter) write(b []byte) (int, int32) {
	p := w.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readers == 0 {
		return 0, abi.EPIPE
	}
	if len(b) == 0 {
		return 0, 0
	}
	space := pipeCapacity - len(p.buf)
	if space <= 0 || (len(b) <= pipeAtomic && space < len(b)) {
		return 0, abi.EAGAIN
	}
	n := min(len(b), space)
	p.buf = append(p.buf, b[:n]...)
	p.n.notify()
	return n, 0
}

func (w *pipeWriter) poll() int16 {
	p := w.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readers == 0 {
		return abi.POLLERR
	}
	if pipeCapacity-len(p.buf) >= pipeAtomic {
		return abi.POLLOUT
	}
	return 0
}

func (w *pipeWriter) changed() <-chan struct{} { return w.p.n.wait() }

func (w *pipeWriter) release() {
	p := w.p
	p.mu.Lock()
	p.writers--
	p.mu.Unlock()
	p.n.notify()
}
