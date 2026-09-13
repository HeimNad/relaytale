package smtpserver

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Limits apply per process. A source is an IPv4 address or IPv6 /64, derived
// only from the transport peer (never from client-supplied SMTP headers).
type Limits struct {
	Connections, PerSource, AuthPerMinute, AuthBurst int
	Lifetime                                         time.Duration
}

func DefaultLimits() Limits { return Limits{128, 8, 60, 20, 5 * time.Minute} }

const maxSources = 4096
const sourceTTL = 5 * time.Minute

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(now time.Time, rate float64, burst int) bool {
	if b.at.IsZero() {
		b.tokens = float64(burst)
	} else {
		b.tokens = min(float64(burst), b.tokens+max(0, now.Sub(b.at).Seconds())*rate)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type sourceState struct {
	connections, auth int
	touched           time.Time
	accepts, attempts bucket
}
type admission struct {
	mu        sync.Mutex
	limits    Limits
	sources   map[string]*sourceState
	total     int
	nextSweep time.Time
	now       func() time.Time
}

func newAdmission(l Limits) *admission {
	return &admission{limits: l, sources: make(map[string]*sourceState), now: time.Now}
}
func sourceKey(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "unknown"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip.WithZone(""), 64).Masked().String()
	}
	return ip.String()
}
func (a *admission) connect(key string) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if !now.Before(a.nextSweep) {
		for key, s := range a.sources {
			if s.connections == 0 && s.auth == 0 && now.Sub(s.touched) >= sourceTTL {
				delete(a.sources, key)
			}
		}
		a.nextSweep = now.Add(time.Minute)
	}
	if a.total >= a.limits.Connections {
		return nil, false
	}
	s := a.sources[key]
	if s == nil {
		if len(a.sources) >= maxSources {
			return nil, false
		}
		s = &sourceState{}
		a.sources[key] = s
	}
	s.touched = now
	if s.connections >= a.limits.PerSource || !s.accepts.take(now, 1, 32) {
		return nil, false
	}
	s.connections++
	a.total++
	var once sync.Once
	return func() {
		once.Do(func() { a.mu.Lock(); defer a.mu.Unlock(); s.connections--; a.total--; s.touched = a.now() })
	}, true
}
func (a *admission) authenticate(key string) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sources[key]
	if s == nil {
		return nil, false
	}
	now := a.now()
	s.touched = now
	// A single source cannot hold all global password-hashing slots. No wait
	// queue is created here: clients receive transient SMTP 454 on saturation.
	if s.auth >= 1 || !s.attempts.take(now, float64(a.limits.AuthPerMinute)/60, a.limits.AuthBurst) {
		return nil, false
	}
	s.auth++
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); defer a.mu.Unlock(); s.auth--; s.touched = a.now() }) }, true
}
