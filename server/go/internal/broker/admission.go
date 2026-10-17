package broker

import (
	"net"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/metrics"
)

// admission 是 MQTT 新连接准入：全局 + 每 IP 两级令牌桶。
//
// 只有全局桶时，一个来源就能把额度打满、让所有合法设备进不来；TLS 握手
// 又发生在认证之前，所以必须在 OnConnect 阶段就用 0x03（server unavailable）
// 拒绝，而不是回 0x05（未授权）——后者会让固件清掉设备密钥。
const (
	perIPRate  = 20 // 每个来源 IP 每秒
	perIPBurst = 20
)

type bucket struct {
	tokens float64
	burst  float64
	last   time.Time
}

func newBucket(rate float64, now time.Time) *bucket {
	return &bucket{tokens: rate, burst: rate, last: now}
}

// take 补充令牌并尝试扣一个；now 必须单调。
func (b *bucket) take(rate float64, now time.Time) bool {
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type admission struct {
	mu     sync.Mutex
	rate   float64
	global *bucket
	ips    map[string]*bucket
	now    func() time.Time
}

func newAdmission(perSec int) *admission {
	if perSec <= 0 {
		perSec = 50
	}
	now := time.Now()
	return &admission{
		rate:   float64(perSec),
		global: newBucket(float64(perSec), now),
		ips:    map[string]*bucket{},
		now:    time.Now,
	}
}

func (a *admission) allow(remote string) bool {
	now := a.now()
	ip := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		ip = host
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.global.take(a.rate, now) {
		metrics.IncRateLimited("mqtt_admit")
		return false
	}
	b, ok := a.ips[ip]
	if !ok {
		b = newBucket(perIPRate, now)
		a.ips[ip] = b
	}
	if !b.take(perIPRate, now) {
		metrics.IncRateLimited("mqtt_admit_ip")
		return false
	}
	a.cleanup(now)
	return true
}

// cleanup 防止攻击者用大量源地址把 map 撑大。超过 4096 个来源时清掉
// 10 分钟没出现过的；清理是摊销的，不在每个连接上全表扫。
func (a *admission) cleanup(now time.Time) {
	if len(a.ips) < 4096 {
		return
	}
	for ip, b := range a.ips {
		if now.Sub(b.last) > 10*time.Minute {
			delete(a.ips, ip)
		}
	}
}
