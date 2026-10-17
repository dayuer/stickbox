// Package registry 记录哪些设备最近发过心跳。
//
// 打印退役后这里不再有每设备 actor 和作业派发，只剩「在线」这一件事：
// 设备在 printer/{dev}/status 上发心跳，收到就记下时间；超过 IdleTimeout
// 没有心跳即判离线。所有状态由一把锁保护，一个后台 goroutine 负责过期。
package registry

import (
	"sync"
	"time"
)

type Options struct {
	// IdleTimeout 是多久没心跳就判离线，默认 120 秒。
	// 设备保证状态一变 2 秒内推、没变也 30 秒一拍，连丢四拍才判离线。
	IdleTimeout time.Duration
	// Ticker 驱动过期检查。nil 时自建 1 秒 ticker；测试里可以手动喂。
	Ticker <-chan time.Time
	Now    func() time.Time
}

type Registry struct {
	mu          sync.Mutex
	lastSeen    map[string]time.Time
	idleTimeout time.Duration
	now         func() time.Time
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
}

func New(o Options) *Registry {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 120 * time.Second
	}
	r := &Registry{
		lastSeen:    map[string]time.Time{},
		idleTimeout: o.IdleTimeout,
		now:         o.Now,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go r.run(o.Ticker)
	return r
}

func (r *Registry) run(tick <-chan time.Time) {
	defer close(r.done)
	if tick == nil {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-r.stop:
			return
		case <-tick:
			r.expire()
		}
	}
}

// Seen 记下一次心跳。设备可能刚上线，也可能本来就在线。
func (r *Registry) Seen(dev string) {
	if dev == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	r.lastSeen[dev] = now
	r.mu.Unlock()
}

func (r *Registry) expire() {
	cutoff := r.now().Add(-r.idleTimeout)
	r.mu.Lock()
	for dev, at := range r.lastSeen {
		if at.Before(cutoff) {
			delete(r.lastSeen, dev)
		}
	}
	r.mu.Unlock()
}

func (r *Registry) Online() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.lastSeen))
	for dev := range r.lastSeen {
		out = append(out, dev)
	}
	return out
}

func (r *Registry) OnlineCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lastSeen)
}

func (r *Registry) IsOnline(dev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.lastSeen[dev]
	return ok
}

func (r *Registry) Shutdown() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}
