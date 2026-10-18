// Package photostatus 接收设备 status 里的 photo 字段。
//
// MQTT 回调在 broker 的任务上，不能在那里做 SQLite 写；这里用一个很短的
// 队列把更新交给后台 worker，满了就丢（下一次心跳 30 秒后还会再来）。
package photostatus

import (
	"log/slog"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

// Status 是设备上报的照片状态。
type Status struct {
	Cap      bool
	Ready    bool
	PLV      int64
	Have     int
	Bad      []string
	FreeKB   int64
	State    string
	InitDone string
}

type Store interface {
	PutDevicePhoto(dev string, p store.DevicePhoto) error
	DevicePhoto(dev string) (store.DevicePhoto, bool, error)
	OwnerOfDevice(dev string) (string, bool, error)
	PhotoInit(userID string) (store.PhotoInit, bool, error)
	ClearPhotoInit(userID string) error
	BumpPlaylistVersion(userID string) (int64, error)
}

// Notifier 让 init_done 后的清单版本变化能推给设备。
type Notifier interface {
	NotifyPlaylist(userID string)
}

type update struct {
	dev string
	st  Status
}

type Service struct {
	st  Store
	nt  Notifier
	now func() time.Time

	ch     chan update
	done   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	latest map[string]Status
}

func New(st Store, nt Notifier) *Service {
	return &Service{
		st: st, nt: nt, now: time.Now,
		ch: make(chan update, 64), done: make(chan struct{}),
		latest: map[string]Status{},
	}
}

func (s *Service) Start() {
	s.wg.Add(1)
	go s.loop()
}

func (s *Service) Close() {
	close(s.done)
	s.wg.Wait()
}

// Update 由 broker 的 MQTT 任务调用；绝不阻塞。
func (s *Service) Update(dev string, st Status) {
	if len(st.Bad) > 8 {
		st.Bad = st.Bad[:8]
	}
	s.mu.Lock()
	s.latest[dev] = st
	s.mu.Unlock()
	select {
	case s.ch <- update{dev: dev, st: st}:
	default:
		// 队列满：丢掉这次落库，内存里的最新值仍然可读。
	}
}

// Get 返回最近一次状态；内存没有时回落到数据库（进程刚重启）。
func (s *Service) Get(dev string) (Status, bool) {
	s.mu.Lock()
	st, ok := s.latest[dev]
	s.mu.Unlock()
	if ok {
		return st, true
	}
	dp, ok, err := s.st.DevicePhoto(dev)
	if err != nil || !ok {
		return Status{}, false
	}
	return Status{
		Cap: dp.Cap, Ready: dp.Ready, PLV: dp.PLV, Have: dp.Have,
		Bad: dp.Bad, FreeKB: dp.FreeKB, State: dp.State, InitDone: dp.InitDone,
	}, true
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			// 尽量把已排队的写完。
			for {
				select {
				case u := <-s.ch:
					s.persist(u)
				default:
					return
				}
			}
		case u := <-s.ch:
			s.persist(u)
		}
	}
}

func (s *Service) persist(u update) {
	owner, bound, err := s.st.OwnerOfDevice(u.dev)
	if err != nil || !bound {
		return
	}
	dp := store.DevicePhoto{
		UserID: owner, Cap: u.st.Cap, Ready: u.st.Ready, PLV: u.st.PLV,
		Have: u.st.Have, Bad: u.st.Bad, FreeKB: u.st.FreeKB,
		State: u.st.State, InitDone: u.st.InitDone, Updated: s.now().Unix(),
	}
	if err := s.st.PutDevicePhoto(u.dev, dp); err != nil {
		slog.Warn("保存设备照片状态失败", "dev", u.dev, "err", err)
		return
	}

	// 设备用掉了一次性初始化 token：立刻作废，并推进 plv 让清单重取。
	if u.st.InitDone == "" {
		return
	}
	in, ok, err := s.st.PhotoInit(owner)
	if err != nil || !ok || len(in.Token) < 8 {
		return
	}
	if in.Token[:8] != u.st.InitDone {
		return
	}
	if err := s.st.ClearPhotoInit(owner); err != nil {
		slog.Warn("清除照片初始化 token 失败", "user", owner, "err", err)
		return
	}
	if _, err := s.st.BumpPlaylistVersion(owner); err != nil {
		slog.Warn("推进照片清单版本失败", "user", owner, "err", err)
		return
	}
	if s.nt != nil {
		s.nt.NotifyPlaylist(owner)
	}
}
