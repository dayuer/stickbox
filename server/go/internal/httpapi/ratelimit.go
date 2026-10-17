package httpapi

import (
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/metrics"
)

// rateLimiter 是进程内滑动窗口计数。服务端重启会清零——对防穷举足够，
// 配对码本身 10 分钟就过期。
type rateLimiter struct {
	name   string
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func newRateLimiter(name string, limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		name: name, limit: limit, window: window,
		hits: map[string][]time.Time{}, now: time.Now,
	}
}

// exceeded 返回该键在当前窗口内是否已经达到上限。
func (l *rateLimiter) exceeded(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	hs := l.prune(key)
	if len(hs) >= l.limit {
		metrics.IncRateLimited(l.name)
		return true
	}
	return false
}

// hit 记一次。调用方先判定要不要计数（例如只对配对失败计数）。
func (l *rateLimiter) hit(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.prune(key), l.now())
}

// allow 是「先计数再看是否超限」的便捷组合。
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	hs := l.prune(key)
	if len(hs) >= l.limit {
		l.hits[key] = hs
		metrics.IncRateLimited(l.name)
		return false
	}
	l.hits[key] = append(hs, l.now())
	return true
}

func (l *rateLimiter) prune(key string) []time.Time {
	cut := l.now().Add(-l.window)
	hs := l.hits[key]
	keep := hs[:0]
	for _, t := range hs {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		// 只在访问时修剪；空键必须删掉，否则来源 IP 越多 map 越涨。
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = keep
	return keep
}
