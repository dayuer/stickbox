package broker

import (
	"sync"
	"time"
)

// pendingLimiter 限制同一台设备的 pending 连接频率。
//
// pending 密钥只代表「知道这个 MAC」。如果合法设备刚好断线重连，
// 攻击者可以在空档里抢先连上；限频把这种窗口压到很小。
type pendingLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newPendingLimiter(limit int, window time.Duration) *pendingLimiter {
	return &pendingLimiter{
		hits:   map[string][]time.Time{},
		limit:  limit,
		window: window,
	}
}

func (l *pendingLimiter) allow(dev string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	hs := l.hits[dev]
	i := 0
	for i < len(hs) && !hs[i].After(cutoff) {
		i++
	}
	hs = hs[i:]
	if len(hs) >= l.limit {
		l.hits[dev] = hs
		return false
	}
	l.hits[dev] = append(hs, now)
	return true
}
