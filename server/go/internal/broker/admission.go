package broker

import (
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/metrics"
)

// admission 是全局新连接准入：令牌桶，最多允许 rate 次/秒。
//
// 服务端重启后所有设备会同时重连。不做准入时，认证队列越排越长，
// 客户端超时重连又把队列放大（T21 实测 2000 台出现 9 万 goroutine）。
// 超限直接拒绝 CONNECT，客户端按自己的退避重试。
type admission struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	burst  float64
	last   time.Time
	now    func() time.Time
}

func newAdmission(perSec int) *admission {
	if perSec <= 0 {
		perSec = 50
	}
	now := time.Now()
	return &admission{
		rate:   float64(perSec),
		tokens: float64(perSec),
		burst:  float64(perSec),
		last:   now,
		now:    time.Now,
	}
}

func (a *admission) allow() bool {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens += now.Sub(a.last).Seconds() * a.rate
	if a.tokens > a.burst {
		a.tokens = a.burst
	}
	a.last = now
	if a.tokens < 1 {
		metrics.IncRateLimited("mqtt_admit")
		return false
	}
	a.tokens--
	return true
}
