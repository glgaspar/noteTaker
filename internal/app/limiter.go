package app

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter counts failures per key in a fixed window, in memory.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*hits
}

type hits struct {
	n     int
	reset time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string]*hits{}}
}

// blocked reports whether key used up its failures, and when it may try again.
func (l *limiter) blocked(key string) (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hits[key]
	if h == nil || time.Now().After(h.reset) {
		return false, time.Time{}
	}
	return h.n >= l.max, h.reset
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10000 { // drop expired entries now and then
		for k, h := range l.hits {
			if now.After(h.reset) {
				delete(l.hits, k)
			}
		}
	}
	h := l.hits[key]
	if h == nil || now.After(h.reset) {
		h = &hits{reset: now.Add(l.window)}
		l.hits[key] = h
	}
	h.n++
}

func (l *limiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// clientIP is the address of the connection, or with TRUST_PROXY=true the address the
// reverse proxy appended to X-Forwarded-For (only enable it behind a proxy that sets it).
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
