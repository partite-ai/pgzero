package kernel

import (
	"encoding/binary"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
)

// poll implements poll(2) over a guest struct pollfd array.
func (p *Process) poll(fdsAddr, nfds uint32, timeoutMs int32) (int32, error) {
	raw, ok := p.mem.Slice(fdsAddr, nfds*abi.SizeofPollfd)
	if !ok {
		return -abi.EFAULT, nil
	}
	var deadline time.Time
	if timeoutMs > 0 {
		deadline = time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	}

	files := make([]*openFile, nfds)
	for i := range files {
		fd := int32(binary.LittleEndian.Uint32(raw[i*abi.SizeofPollfd+abi.OffPollfdFD:]))
		if fd >= 0 {
			files[i], _ = p.fds.get(fd) // nil: POLLNVAL
		}
	}

	chs := make([]<-chan struct{}, 0, nfds)
	for {
		// Take the change channels before checking, so a change between
		// the check and the wait is not missed.
		chs = chs[:0]
		for _, of := range files {
			if of != nil {
				chs = append(chs, of.f.changed())
			}
		}
		ready := int32(0)
		for i, of := range files {
			ent := raw[i*abi.SizeofPollfd:]
			fd := int32(binary.LittleEndian.Uint32(ent[abi.OffPollfdFD:]))
			events := int16(binary.LittleEndian.Uint16(ent[abi.OffPollfdEvents:]))
			var revents int16
			switch {
			case fd < 0:
			case of == nil:
				revents = abi.POLLNVAL
			default:
				revents = of.f.poll() & (events | abi.POLLERR | abi.POLLHUP)
			}
			binary.LittleEndian.PutUint16(ent[abi.OffPollfdRevents:], uint16(revents))
			if revents != 0 {
				ready++
			}
		}
		if ready > 0 || timeoutMs == 0 {
			return ready, nil
		}
		switch p.block(deadline, chs...) {
		case interrupted:
			return -abi.EINTR, nil
		case killed:
			return 0, errKilled
		case timedOut:
			return 0, nil
		}
	}
}
