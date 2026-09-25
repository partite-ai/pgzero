package kernel

import "sync"

// notifier wakes everyone waiting for a state change. Waiters take the
// current channel with wait() before re-checking their condition; notify()
// closes it and starts a new one, so no wakeup is ever lost.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

func (n *notifier) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}
