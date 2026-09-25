package kernel

import (
	"io"
	"sync"

	"github.com/partite-ai/pgzero/internal/abi"
)

// readerFile adapts an io.Reader (the kernel's stdin) to a file. A
// goroutine reads ahead into a bounded buffer so reads can be
// non-blocking.
type readerFile struct {
	mu   sync.Mutex
	buf  []byte
	eof  bool
	room *sync.Cond
	n    notifier
}

const readerAhead = 64 << 10

func newReaderFile(r io.Reader) *readerFile {
	f := &readerFile{}
	f.room = sync.NewCond(&f.mu)
	go f.pump(r)
	return f
}

func (f *readerFile) pump(r io.Reader) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		f.mu.Lock()
		if n > 0 {
			f.buf = append(f.buf, chunk[:n]...)
		}
		if err != nil {
			f.eof = true
		}
		for len(f.buf) >= readerAhead && !f.eof {
			f.room.Wait()
		}
		done := f.eof
		f.mu.Unlock()
		f.n.notify()
		if done {
			return
		}
	}
}

func (f *readerFile) read(b []byte) (int, int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buf) > 0 {
		n := copy(b, f.buf)
		f.buf = f.buf[n:]
		f.room.Signal()
		return n, 0
	}
	if f.eof {
		return 0, 0
	}
	return 0, abi.EAGAIN
}

func (f *readerFile) write([]byte) (int, int32) { return 0, abi.EBADF }

func (f *readerFile) poll() int16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buf) > 0 {
		return abi.POLLIN
	}
	if f.eof {
		return abi.POLLHUP
	}
	return 0
}

func (f *readerFile) changed() <-chan struct{} { return f.n.wait() }

// release stops nothing: the pump goroutine ends at EOF of the kernel's
// stdin, which the kernel does not own.
func (f *readerFile) release() {}

// writerFile adapts an io.Writer (the kernel's stdout/stderr). Writes are
// synchronous and it is always writable.
type writerFile struct {
	mu sync.Mutex
	w  io.Writer
}

func newWriterFile(w io.Writer) *writerFile { return &writerFile{w: w} }

func (f *writerFile) read([]byte) (int, int32) { return 0, abi.EBADF }

func (f *writerFile) write(b []byte) (int, int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.w.Write(b)
	if err != nil && n == 0 {
		return 0, abi.EIO
	}
	return n, 0
}

func (f *writerFile) poll() int16              { return abi.POLLOUT }
func (f *writerFile) changed() <-chan struct{} { return nil }
func (f *writerFile) release()                 {}

// nullFile is /dev/null.
type nullFile struct{}

func (nullFile) read([]byte) (int, int32)    { return 0, 0 }
func (nullFile) write(b []byte) (int, int32) { return len(b), 0 }
func (nullFile) poll() int16                 { return abi.POLLIN | abi.POLLOUT }
func (nullFile) changed() <-chan struct{}    { return nil }
func (nullFile) release()                    {}
