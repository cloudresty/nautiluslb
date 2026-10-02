package discovery

import "sync"

// warnTracker remembers what has been warned about, so a misconfigured
// Service is reported once rather than on every 30s pass. A changed value
// (the annotation was edited, still wrongly) warns again.
//
// Keys touched during a pass are remembered so endPass can drop the ones whose
// Service is gone, keeping the tracker bounded under Service churn.
type warnTracker struct {
	mu      sync.Mutex
	seen    map[string]string
	touched map[string]struct{}
}

func newWarnTracker() *warnTracker {
	return &warnTracker{seen: make(map[string]string), touched: make(map[string]struct{})}
}

// beginPass starts tracking which keys a discovery pass touches.
func (w *warnTracker) beginPass() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.touched = make(map[string]struct{})
}

// endPass finishes a pass. With prune set, keys not touched during the pass are
// dropped. A pass that could not list every Service must not prune: the
// Services it missed would warn again on the next pass.
func (w *warnTracker) endPass(prune bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if prune {
		for key := range w.seen {
			if _, ok := w.touched[key]; !ok {
				delete(w.seen, key)
			}
		}
	}
	w.touched = make(map[string]struct{})
}

// shouldWarn reports whether key has not yet been warned about with value,
// and records it.
func (w *warnTracker) shouldWarn(key, value string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.touched[key] = struct{}{}
	if old, ok := w.seen[key]; ok && old == value {
		return false
	}
	w.seen[key] = value
	return true
}
