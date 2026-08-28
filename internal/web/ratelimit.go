package web

import (
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// limiter is a small in-memory token bucket per client IP. It exists to make
// guessing a 256-bit RSVP token or brute-forcing the login pointless, not to
// shape real traffic.
type limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
	lastGC   time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newLimiter(capacity, refillPerSecond float64) *limiter {
	return &limiter{
		buckets:  map[string]*bucket{},
		capacity: capacity,
		refill:   refillPerSecond,
		lastGC:   time.Now(),
	}
}

// allow consumes a token for key, reporting whether the request may proceed.
func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, seen: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * l.refill
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP extracts the caller's address, honouring X-Forwarded-For only when
// the deployment says it sits behind a proxy. Trusting it unconditionally would
// let anyone reset their own rate limit with a header.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if i := indexByte(fwd, ','); i > 0 {
				return trimSpace(fwd[:i])
			}
			return trimSpace(fwd)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
