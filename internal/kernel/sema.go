package kernel

import (
	"sync"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
)

// semaphore is a counting semaphore shared by all processes.
type semaphore struct {
	mu    sync.Mutex
	count int32
	n     notifier
}

type semaTable struct {
	mu   sync.Mutex
	sems map[int32]*semaphore
	next int32
}

func (t *semaTable) init() {
	t.sems = map[int32]*semaphore{}
	t.next = 1
}

func (t *semaTable) create(initial int32) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	t.sems[id] = &semaphore{count: initial}
	return id
}

func (t *semaTable) get(id int32) *semaphore {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sems[id]
}

func (s *semaphore) tryLock() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count > 0 {
		s.count--
		return true
	}
	return false
}

func (p *Process) semaLock(s *semaphore) (int32, error) {
	for {
		ch := s.n.wait()
		if s.tryLock() {
			return 0, nil
		}
		switch p.block(time.Time{}, ch) {
		case interrupted:
			return -abi.EINTR, nil
		case killed:
			return 0, errKilled
		}
	}
}

func (s *semaphore) unlock() {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	s.n.notify()
}

func (s *semaphore) reset() {
	s.mu.Lock()
	s.count = 0
	s.mu.Unlock()
}
