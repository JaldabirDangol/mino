package auth

import (
	"sync"
	"time"
)

// Limiter is a fixed-window, in-memory rate limiter. Expired windows are swept lazily on
// access, so it needs no background goroutine.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	count int
	reset time.Time
}

// NewLimiter allows limit events per key within window.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, entries: map[string]*bucket{}, now: time.Now}
}

// Allow records an event for key and reports whether it is within the limit.
// When it is not, retryAfter tells when the window resets.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.entries) > 10_000 {
		for k, b := range l.entries {
			if now.After(b.reset) {
				delete(l.entries, k)
			}
		}
	}
	b, found := l.entries[key]
	if !found || now.After(b.reset) {
		b = &bucket{reset: now.Add(l.window)}
		l.entries[key] = b
	}
	if b.count >= l.limit {
		return false, b.reset.Sub(now)
	}
	b.count++
	return true, 0
}

// Reset clears the counter of key (e.g. after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}
