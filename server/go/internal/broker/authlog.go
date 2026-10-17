package broker

import (
	"sort"
	"sync"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// AuthEvent 是一次 MQTT CONNECT 的结果，设备诊断用。只保留最近若干条，
// 重启即清空；不记录任何密钥材料。
type AuthEvent struct {
	At     time.Time `json:"at"`
	Dev    string    `json:"dev"`
	Code   int       `json:"code"`   // CONNACK 返回码：0x00/0x03/0x05
	Result string    `json:"result"` // ok / invalid_key / db_error / pending_rate ...
	Remote string    `json:"remote,omitempty"`
}

type authLog struct {
	mu  sync.Mutex
	evs []AuthEvent
	max int
}

func newAuthLog(max int) *authLog {
	if max <= 0 {
		max = 200
	}
	return &authLog{max: max}
}

func (l *authLog) add(ev AuthEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
	if len(l.evs) > l.max {
		l.evs = append([]AuthEvent(nil), l.evs[len(l.evs)-l.max:]...)
	}
}

// recent 返回该设备最近的 n 条事件，新的在前。
func (l *authLog) recent(dev string, n int) []AuthEvent {
	if n <= 0 {
		n = 10
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuthEvent, 0, n)
	for i := len(l.evs) - 1; i >= 0 && len(out) < n; i-- {
		if dev == "" || l.evs[i].Dev == dev {
			out = append(out, l.evs[i])
		}
	}
	return out
}

// connectedKeys 返回当前在线连接使用的 key_id（去重排序）。
func (h *Hook) connectedKeys(dev string) []string {
	h.ids.mu.RLock()
	defer h.ids.mu.RUnlock()
	set := map[string]bool{}
	for _, id := range h.ids.m {
		if id.Role == auth.RoleDevice && id.Dev == dev && id.KeyID != "" {
			set[id.KeyID] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (h *Hook) noteAuth(cl *mqtt.Client, dev string, code int, result string) {
	if h.auth == nil {
		return
	}
	h.auth.add(AuthEvent{
		At: time.Now(), Dev: dev, Code: code, Result: result, Remote: cl.Net.Remote,
	})
}
