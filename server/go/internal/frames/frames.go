// Package frames 维护每个用户的当前帧包并负责下发。
//
// 渲染是重活（T13a 一帧约 600ms），HTTP 请求只给用户标脏就返回；
// 每个用户一个后台 worker，防抖 2 秒合并多次上报，全局限制渲染并发。
//
// 帧包同时落盘到 <root>/frames/<userID>/frame.cache，进程重启直接
// 从磁盘加载，不重新渲染。渲染/落盘失败时保留上一份好帧。
package frames

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

const (
	// 下发给设备的帧包：magic + 三段长度 + base + atlas + meta。
	packMagic  = "SBF1"
	packHeader = 16

	// 落盘缓存 v3：magic + version + built_at + received_at + used_at + packLen + pack。
	// v2 没有 used_at，读取时用 built_at 兜底。
	cacheMagic  = "SBC3"
	cacheMagic2 = "SBC2"
	cacheHeader = 40

	// MaxPackBytes 是设备一个 256KB flash 槽能放下的帧包上限：
	// 262144 - 32 字节槽头 = 262112。见 T13b rom/main/framepack.h 的 FP_MAX_PACK。
	MaxPackBytes = 262112
	maxPartBytes = 1 << 20
)

var (
	// ErrNotPaired 表示设备密钥还没完成配对（或已吊销）。
	ErrNotPaired = errors.New("frames: device not paired")
	// ErrNoFrame 表示设备已配对，但还没有可下发的帧。
	ErrNoFrame = errors.New("frames: frame not ready")

	errNoDevices = errors.New("frames: no devices")
)

// Store 是帧服务对 store 的窄依赖。
type Store interface {
	DevicesOfUser(userID string) ([]string, error)
	OwnerOfDevice(dev string) (string, bool, error)
	// ListUsers 用于启动/巡检时给「已配对但还没有帧缓存」的老用户补帧。
	ListUsers() ([]store.User, error)
	// KeyByID 是设备端点判权的唯一依据：必须用令牌自己的 KeyID
	// 查 pending/disabled/user_id，不能按 dev 反查主人。
	KeyByID(keyID string) (store.Key, bool, error)
}

// Usage 提供用户最近一次上报。
type Usage interface {
	Latest(userID string) (usage.Report, time.Time, bool, error)
}

// Publisher 把帧版本通知推给一台设备。
type Publisher interface {
	PublishFrame(dev string, payload []byte) error
}

// Builder 把快照渲染成帧包。单测里换成假实现，避免真的跑渲染器。
type Builder interface {
	Build(source.Snapshot) (*frame.Bundle, error)
}

// RenderBuilder 把 T13a 的渲染器接到帧服务上。
type RenderBuilder struct{ R *render.Renderer }

func (b RenderBuilder) Build(s source.Snapshot) (*frame.Bundle, error) {
	return frame.Build(b.R, s)
}

type Options struct {
	DefaultTZ         string        // agent 没上报时区时的兜底
	Debounce          time.Duration // 同一用户多次标脏的合并窗口
	MaxWait           time.Duration // 防抖硬上限，避免持续上报把渲染饿死
	RenderConcurrency int           // 全局同时渲染数
	StaleAfter        time.Duration // 超过这么久没上报就重渲一次（更新「X 分前」）
	StaleCheck        time.Duration // 检查跨天/过期的频率
	// CacheMaxBytes / MemoryMaxBytes 是磁盘与常驻内存上限；按最近取用淘汰。
	CacheMaxBytes  int64
	MemoryMaxBytes int64
	// BackfillPerSec 是启动补帧速率（用户/秒）。
	BackfillPerSec int
	Now            func() time.Time
	// After 是防抖计时器；测试注入假时钟，默认 time.After。
	After func(time.Duration) <-chan time.Time
}

type cached struct {
	data     []byte // nil = 只在磁盘上，取用时再读
	ver      uint64
	at       int64 // 构建时刻（Unix 秒）
	received int64 // 这次渲染用的用量到达时刻（Unix 秒）；0 = 还没有用量
	used     int64 // 最近一次被设备取用的时刻（Unix 秒），LRU 依据
	size     int64 // 帧包字节数（不含缓存头）
}

type worker struct {
	ch     chan struct{}
	cancel context.CancelFunc
	active bool // 有未完成的标记（排队/防抖/渲染）
}

// Service 维护全部用户的当前帧。
type Service struct {
	root    string
	store   Store
	usage   Usage
	pub     Publisher
	builder Builder
	opt     Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	workersMu sync.Mutex
	workers   map[string]*worker
	pending   int // 有排队/防抖/渲染任务的用户数

	renderSem chan struct{}
	commitMu  sync.Mutex // 串行提交，保证版本号按提交顺序递增

	mu    sync.RWMutex
	cache map[string]*cached

	// 常驻内存 LRU：cache 里的 data 只有最近用过的用户才有。
	memLRU    *list.List // 值 = userID；front 最近使用
	memIndex  map[string]*list.Element
	memBytes  int64
	diskBytes int64

	// 启动补帧队列：限速逐个 MarkDirty，避免重连时一次性渲染所有用户。
	backfillMu  sync.Mutex
	backfillQ   []string
	backfillSet map[string]struct{}
}

func New(root string, st Store, us Usage, pub Publisher, b Builder, opt Options) (*Service, error) {
	if root == "" {
		return nil, errors.New("frames: root 为空")
	}
	if st == nil || us == nil || pub == nil || b == nil {
		return nil, errors.New("frames: 缺依赖")
	}
	if opt.DefaultTZ == "" {
		opt.DefaultTZ = "UTC"
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 2 * time.Second
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 10 * time.Second
	}
	if opt.RenderConcurrency <= 0 {
		opt.RenderConcurrency = 2
	}
	if opt.StaleAfter <= 0 {
		opt.StaleAfter = 15 * time.Minute
	}
	if opt.StaleCheck <= 0 {
		opt.StaleCheck = time.Minute
	}
	if opt.CacheMaxBytes <= 0 {
		opt.CacheMaxBytes = 256 << 20
	}
	if opt.MemoryMaxBytes <= 0 {
		opt.MemoryMaxBytes = 64 << 20
	}
	if opt.BackfillPerSec <= 0 {
		opt.BackfillPerSec = 2
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.After == nil {
		opt.After = time.After
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		root: root, store: st, usage: us, pub: pub, builder: b, opt: opt,
		ctx: ctx, cancel: cancel,
		workers:     map[string]*worker{},
		renderSem:   make(chan struct{}, opt.RenderConcurrency),
		cache:       map[string]*cached{},
		memLRU:      list.New(),
		memIndex:    map[string]*list.Element{},
		backfillSet: map[string]struct{}{},
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		cancel()
		return nil, fmt.Errorf("frames: 创建缓存目录：%w", err)
	}
	s.load()
	return s, nil
}

// PendingUsers 是当前真正有排队/防抖/渲染任务的用户数。
// 注意不是 worker 总数：worker 会常驻，空闲时不能算排队。
func (s *Service) PendingUsers() int {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.pending
}

// Close 停掉所有 worker。服务退出时调用。
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) dir() string              { return filepath.Join(s.root, "frames") }
func (s *Service) userDir(id string) string { return filepath.Join(s.dir(), id) }
func (s *Service) cacheFile(id string) string {
	return filepath.Join(s.userDir(id), "frame.cache")
}

// safeUserID 只挡路径分隔符和越界长度；store 生成的是 32 位十六进制。
func safeUserID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// load 只读缓存头，不把帧包读进内存：进程重启后内存里只有元数据，
// 哪个用户第一次被取帧，哪个用户的帧包才从磁盘读上来。
func (s *Service) load() {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		slog.Warn("读帧缓存目录失败", "dir", s.dir(), "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !safeUserID(e.Name()) {
			continue
		}
		path := filepath.Join(s.dir(), e.Name(), "frame.cache")
		c, err := readCacheHeader(path)
		if err != nil {
			slog.Warn("帧缓存损坏，忽略", "user", e.Name(), "err", err)
			continue
		}
		s.cache[e.Name()] = c
		s.diskBytes += c.size + cacheHeader
	}
	// 启动时磁盘已超限：按 used 淘汰。
	s.mu.Lock()
	victims := s.evictDiskLocked("", s.opt.CacheMaxBytes)
	s.mu.Unlock()
	for _, id := range victims {
		os.RemoveAll(s.userDir(id))
	}
}

// MarkDirty 标记用户数据变了，后台 worker 会在防抖窗口后重渲染。
// HTTP 请求只调用这个，不等待渲染。
func (s *Service) MarkDirty(userID string) {
	if !safeUserID(userID) {
		return
	}
	select {
	case <-s.ctx.Done():
		return // 服务已关闭
	default:
	}
	s.workersMu.Lock()
	w, ok := s.workers[userID]
	if !ok {
		wctx, cancel := context.WithCancel(s.ctx)
		w = &worker{ch: make(chan struct{}, 1), cancel: cancel, active: true}
		s.workers[userID] = w
		s.pending++
		s.wg.Add(1)
		go s.workerLoop(wctx, userID, w)
	} else if !w.active {
		w.active = true
		s.pending++
	}
	s.workersMu.Unlock()
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

func (s *Service) workerLoop(ctx context.Context, userID string, w *worker) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.ch:
		}

		// 防抖：窗口内再来标脏就重置计时，合并成一次渲染。
		// maxWait 是硬上限：持续每 2 秒上报时不能无限重置、永远不出帧。
		start := s.opt.Now()
		wait := s.opt.Debounce
		if wait > s.opt.MaxWait {
			wait = s.opt.MaxWait
		}
	debounce:
		for {
			timer := s.opt.After(wait)
			select {
			case <-ctx.Done():
				return
			case <-w.ch:
				if since := s.opt.Now().Sub(start); since >= s.opt.MaxWait {
					break debounce
				} else {
					wait = s.opt.Debounce
					if remaining := s.opt.MaxWait - since; remaining < wait {
						wait = remaining
					}
				}
			case <-timer:
				break debounce
			}
		}

		select {
		case s.renderSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		err := s.Rebuild(userID)
		<-s.renderSem

		if errors.Is(err, errNoDevices) {
			s.workersMu.Lock()
			if s.workers[userID] == w {
				delete(s.workers, userID)
				if w.active {
					w.active = false
					s.pending--
				}
			}
			s.workersMu.Unlock()
			return
		}
		if err != nil {
			slog.Warn("重建帧失败，保留旧帧", "user", userID, "err", err)
		}

		// 渲染期间来的上报已经进了 ch：保持 active，让循环顶部去消费；
		// 这里不能把信号取走，否则循环会永远等在下一次上报上。
		s.workersMu.Lock()
		if len(w.ch) == 0 && w.active {
			w.active = false
			s.pending--
		}
		s.workersMu.Unlock()
	}
}

// Rebuild 同步渲染一个用户的帧。生产路径由 worker 调用，单测/运维也可直接调。
func (s *Service) Rebuild(userID string) error {
	if !safeUserID(userID) {
		return fmt.Errorf("frames: 非法 userID %q", userID)
	}
	devs, err := s.store.DevicesOfUser(userID)
	if err != nil {
		return fmt.Errorf("frames: 查设备：%w", err)
	}
	if len(devs) == 0 {
		return errNoDevices
	}
	rep, received, ok, err := s.usage.Latest(userID)
	if err != nil {
		return fmt.Errorf("frames: 读用量：%w", err)
	}
	loc := s.locationFor(rep.TZ)
	now := s.opt.Now()

	var snap source.Snapshot
	if ok {
		snap, err = source.Claude{Report: rep, ReceivedAt: received}.Snapshot(context.Background(), now.In(loc))
	} else {
		// 刚配对、agent 还没上报过：也要出一张「等待上报」的帧。
		snap, err = source.Claude{}.Snapshot(context.Background(), now.In(loc))
	}
	if err != nil {
		return fmt.Errorf("frames: 生成快照：%w", err)
	}

	bun, err := s.builder.Build(snap)
	if err != nil {
		slog.Warn("渲染帧失败，保留旧帧", "user", userID, "err", err)
		return fmt.Errorf("frames: 渲染：%w", err)
	}
	data, err := Pack(bun)
	if err != nil {
		slog.Warn("打包帧失败，保留旧帧", "user", userID, "err", err)
		return fmt.Errorf("frames: 打包：%w", err)
	}
	return s.commit(userID, data, now, received, ok)
}

// commit 写临时文件（含 fsync）在提交锁内、读锁外；内存里只做 rename 和替换。
// 比当前帧更旧的快照直接丢弃，避免并发上报时旧数据覆盖新数据。
func (s *Service) commit(userID string, data []byte, now time.Time, received time.Time, hasUsage bool) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	s.mu.RLock()
	cur := s.cache[userID]
	s.mu.RUnlock()
	if cur != nil && hasUsage && received.Unix() < cur.received {
		return nil
	}
	// 渲染期间可能刚好被解绑：名下没设备就不要再把缓存写回来。
	devs, err := s.store.DevicesOfUser(userID)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return errNoDevices
	}

	ver := uint64(1)
	if cur != nil {
		ver = cur.ver + 1
	}
	rec := int64(0)
	if hasUsage {
		rec = received.Unix()
	}
	c := &cached{data: data, ver: ver, at: now.Unix(), received: rec}

	tmp, err := s.writeTemp(userID, c)
	if err != nil {
		return fmt.Errorf("frames: 写缓存：%w", err)
	}

	s.mu.Lock()
	if err := os.Rename(tmp, s.cacheFile(userID)); err != nil {
		s.mu.Unlock()
		os.Remove(tmp)
		return err
	}
	c.used = now.Unix()
	c.size = int64(len(data))
	s.cache[userID] = c
	if cur != nil {
		s.diskBytes += c.size - cur.size
	} else {
		s.diskBytes += c.size + cacheHeader
	}
	s.touchMemLocked(userID, c)
	victims := s.evictDiskLocked(userID, s.opt.CacheMaxBytes)
	s.mu.Unlock()

	for _, id := range victims {
		if err := os.RemoveAll(s.userDir(id)); err != nil {
			slog.Warn("淘汰帧缓存失败", "user", id, "err", err)
		}
	}
	if err := syncDir(s.userDir(userID)); err != nil {
		slog.Warn("同步帧缓存目录失败", "user", userID, "err", err)
	}
	s.notify(userID, ver)
	return nil
}

// touchMemLocked 把用户帧包放进常驻内存 LRU，并淘汰最久没被取用的。
// 要求持有 s.mu。
func (s *Service) touchMemLocked(userID string, c *cached) {
	if c.data == nil {
		return
	}
	if e, ok := s.memIndex[userID]; ok {
		s.memLRU.MoveToFront(e)
		return
	}
	e := s.memLRU.PushFront(userID)
	s.memIndex[userID] = e
	s.memBytes += c.size
	for s.memBytes > s.opt.MemoryMaxBytes && s.memLRU.Len() > 1 {
		back := s.memLRU.Back()
		id := back.Value.(string)
		s.memLRU.Remove(back)
		delete(s.memIndex, id)
		if old, ok := s.cache[id]; ok && old.data != nil {
			s.memBytes -= old.size
			old.data = nil
		}
	}
}

// evictDiskLocked 按 used 从旧到新删除，直到磁盘总量回到上限内。
// 返回需要删除目录的 userID；要求持有 s.mu。
func (s *Service) evictDiskLocked(keep string, max int64) []string {
	var victims []string
	for s.diskBytes > max {
		var oldestID string
		var oldest int64
		for id, c := range s.cache {
			if id == keep {
				continue
			}
			if oldestID == "" || c.used < oldest {
				oldestID, oldest = id, c.used
			}
		}
		if oldestID == "" {
			break
		}
		old := s.cache[oldestID]
		delete(s.cache, oldestID)
		s.diskBytes -= old.size + cacheHeader
		if e, ok := s.memIndex[oldestID]; ok {
			s.memLRU.Remove(e)
			delete(s.memIndex, oldestID)
			if old.data != nil {
				s.memBytes -= old.size
			}
		}
		victims = append(victims, oldestID)
	}
	return victims
}

func (s *Service) writeTemp(userID string, c *cached) (string, error) {
	dir := s.userDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	buf := encodeCache(c)
	tmp, err := os.CreateTemp(dir, ".frame-*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// notify 把新版本发给该用户名下所有设备。失败不影响帧本身，
// 设备重连订阅时会由 Resend 补上。
func (s *Service) notify(userID string, ver uint64) {
	// 并发重建时，旧版本的通知可能比新版本晚到；只发当前版本。
	if cur, ok := s.cachedVersion(userID); !ok || cur != ver {
		return
	}
	devs, err := s.store.DevicesOfUser(userID)
	if err != nil {
		slog.Warn("列设备失败，跳过帧通知", "user", userID, "err", err)
		return
	}
	payload := versionPayload(ver)
	for _, dev := range devs {
		if err := s.pub.PublishFrame(dev, payload); err != nil {
			slog.Warn("推送帧版本失败", "dev", dev, "ver", ver, "err", err)
		}
	}
}

func versionPayload(ver uint64) []byte {
	b, _ := json.Marshal(struct {
		Ver uint64 `json:"ver"`
	}{ver})
	return b
}

func (s *Service) cachedVersion(userID string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cache[userID]
	if !ok {
		return 0, false
	}
	return c.ver, true
}

// Current 返回用户的当前帧包与版本。返回的字节是共享缓存，调用方不要改。
//
// 帧包不常驻内存：不在 LRU 里时从磁盘读一次，同时更新 used 供淘汰。
func (s *Service) Current(userID string) (data []byte, ver uint64, ok bool) {
	s.mu.Lock()
	c, ok := s.cache[userID]
	if !ok {
		s.mu.Unlock()
		return nil, 0, false
	}
	needRead := c.data == nil
	s.mu.Unlock()

	if needRead {
		got, err := readPackFile(s.cacheFile(userID))
		if err != nil {
			slog.Warn("读帧缓存失败", "user", userID, "err", err)
			return nil, 0, false
		}
		s.mu.Lock()
		c, ok = s.cache[userID]
		if !ok {
			s.mu.Unlock()
			return nil, 0, false
		}
		if c.data == nil {
			c.data = got
			c.size = int64(len(got))
			s.touchMemLocked(userID, c)
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	c, ok = s.cache[userID]
	if !ok {
		s.mu.Unlock()
		return nil, 0, false
	}
	if e, ok := s.memIndex[userID]; ok {
		s.memLRU.MoveToFront(e)
	}
	c.used = s.opt.Now().Unix()
	data, ver = c.data, c.ver
	s.mu.Unlock()
	return data, ver, true
}

// readPackFile 读取并校验磁盘缓存，返回帧包字节。
func readPackFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := decodeCache(raw)
	if err != nil {
		return nil, err
	}
	return c.data, nil
}

// CurrentForKey 按令牌自己的 KeyID 判权：未配对、已吊销、角色不对、
// dev 不符或没有 user_id 都返回 ErrNotPaired。不要按 dev 反查主人。
func (s *Service) CurrentForKey(keyID, dev string) ([]byte, uint64, error) {
	k, found, err := s.store.KeyByID(keyID)
	if err != nil {
		return nil, 0, err
	}
	if !found || k.Disabled || k.Pending || k.Role != "device" ||
		k.Dev != dev || k.UserID == "" {
		return nil, 0, ErrNotPaired
	}
	data, ver, ok := s.Current(k.UserID)
	if !ok {
		// 懒加载：设备第一次取帧时才为这个用户渲染，不在启动时全量补帧。
		s.MarkDirty(k.UserID)
		return nil, 0, ErrNoFrame
	}
	return data, ver, nil
}

// Resend 在设备订阅 frame 主题时补发当前版本。
func (s *Service) Resend(dev string) {
	owner, bound, err := s.store.OwnerOfDevice(dev)
	if err != nil || !bound {
		return
	}
	ver, ok := s.cachedVersion(owner)
	if !ok {
		// 设备订阅了 frame 但还没有帧：懒加载，不等启动补帧队列。
		s.MarkDirty(owner)
		return
	}
	if err := s.pub.PublishFrame(dev, versionPayload(ver)); err != nil {
		slog.Warn("补发帧版本失败", "dev", dev, "err", err)
	}
}

// DeviceRemoved 在解绑后调用：名下没有设备了就清掉帧缓存，不做重渲染。
func (s *Service) DeviceRemoved(userID string) {
	devs, err := s.store.DevicesOfUser(userID)
	if err != nil {
		return
	}
	if len(devs) == 0 {
		s.drop(userID)
	}
}

func (s *Service) drop(userID string) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	s.mu.Lock()
	c, had := s.cache[userID]
	if had {
		s.diskBytes -= c.size + cacheHeader
		delete(s.cache, userID)
	}
	if e, ok := s.memIndex[userID]; ok {
		s.memLRU.Remove(e)
		delete(s.memIndex, userID)
		if had && c.data != nil {
			s.memBytes -= c.size
		}
	}
	s.mu.Unlock()

	s.workersMu.Lock()
	if w, ok := s.workers[userID]; ok {
		w.cancel()
		delete(s.workers, userID)
	}
	s.workersMu.Unlock()

	if err := os.RemoveAll(s.userDir(userID)); err != nil {
		slog.Warn("清理帧缓存失败", "user", userID, "err", err)
	}
}

// Run 每分钟检查一次跨天与「超过 15 分钟没上报」，按用户时区判断。
func (s *Service) Run(ctx context.Context) {
	go s.backfillLoop(ctx)
	s.checkStale(s.opt.Now())
	t := time.NewTicker(s.opt.StaleCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.checkStale(s.opt.Now())
		}
	}
}

func (s *Service) checkStale(now time.Time) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.cache))
	for id := range s.cache {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		devs, err := s.store.DevicesOfUser(id)
		if err != nil {
			continue
		}
		if len(devs) == 0 {
			s.drop(id)
			continue
		}
		rep, received, ok, err := s.usage.Latest(id)
		if err != nil {
			continue
		}
		loc := s.locationFor(rep.TZ)

		s.mu.RLock()
		c := s.cache[id]
		s.mu.RUnlock()
		if c == nil {
			continue
		}
		built := time.Unix(c.at, 0).In(loc)
		if built.Format("2006-01-02") != now.In(loc).Format("2006-01-02") {
			s.MarkDirty(id) // 跨天：日期/星期变了
			continue
		}
		// 超过 15 分钟没上报：重渲一次，让「更新 X 分前」和告警色跟上；
		// 之后每 15 分钟一次（帧的 at 会更新）。
		if ok && now.Sub(received) >= s.opt.StaleAfter &&
			now.Sub(time.Unix(c.at, 0)) >= s.opt.StaleAfter {
			s.MarkDirty(id)
		}
	}

	// 新版本部署/进程重启时，已配对的老用户没有帧缓存，上面的循环看不到。
	// 不在启动时一次性渲染：设备第一次 GET/订阅会懒加载，其余用户进入
	// 限速补帧队列（默认 2 用户/秒），避免重连风暴叠加渲染风暴。
	users, err := s.store.ListUsers()
	if err != nil {
		slog.Warn("枚举用户失败，跳过补帧", "err", err)
		return
	}
	var missing []string
	for _, u := range users {
		if u.Disabled {
			continue
		}
		s.mu.RLock()
		_, cached := s.cache[u.ID]
		s.mu.RUnlock()
		if cached {
			continue
		}
		devs, err := s.store.DevicesOfUser(u.ID)
		if err != nil || len(devs) == 0 {
			continue
		}
		missing = append(missing, u.ID)
	}
	s.enqueueBackfill(missing)
}

// enqueueBackfill 把缺帧用户放进限速队列，去重。
func (s *Service) enqueueBackfill(ids []string) {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	for _, id := range ids {
		if _, ok := s.backfillSet[id]; ok {
			continue
		}
		s.backfillSet[id] = struct{}{}
		s.backfillQ = append(s.backfillQ, id)
	}
}

// backfillLoop 按 BackfillPerSec 逐个标脏；有帧了就跳过。
func (s *Service) backfillLoop(ctx context.Context) {
	interval := time.Second / time.Duration(s.opt.BackfillPerSec)
	if interval <= 0 {
		interval = time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.backfillMu.Lock()
		var id string
		for len(s.backfillQ) > 0 && id == "" {
			id = s.backfillQ[0]
			s.backfillQ = s.backfillQ[1:]
			delete(s.backfillSet, id)
		}
		s.backfillMu.Unlock()
		if id == "" {
			continue
		}
		s.mu.RLock()
		_, cached := s.cache[id]
		s.mu.RUnlock()
		if cached {
			continue
		}
		s.MarkDirty(id)
	}
}

func (s *Service) locationFor(tz string) *time.Location {
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
		slog.Warn("未知时区，回退默认", "tz", tz)
	}
	if s.opt.DefaultTZ != "" {
		if l, err := time.LoadLocation(s.opt.DefaultTZ); err == nil {
			return l
		}
	}
	return time.UTC
}

// Pack 生成下发格式：
//
//	"SBF1" | baseLen uint32 | atlasLen uint32 | metaLen uint32 | base | atlas | meta
//
// 三个长度都是大端。总长超过设备槽上限直接失败，避免下发装不下的帧包。
func Pack(b *frame.Bundle) ([]byte, error) {
	if b == nil {
		return nil, errors.New("frames: nil bundle")
	}
	meta, err := b.MetaJSON()
	if err != nil {
		return nil, err
	}
	if len(b.Base) == 0 || len(b.Atlas) == 0 || len(meta) == 0 {
		return nil, errors.New("frames: 帧包缺段")
	}
	if len(b.Base) > maxPartBytes || len(b.Atlas) > maxPartBytes || len(meta) > maxPartBytes {
		return nil, errors.New("frames: 帧包某段过大")
	}
	total := packHeader + len(b.Base) + len(b.Atlas) + len(meta)
	if total > MaxPackBytes {
		return nil, fmt.Errorf("frames: 帧包 %d 字节超过设备槽上限 %d", total, MaxPackBytes)
	}
	out := make([]byte, total)
	copy(out[:4], packMagic)
	binary.BigEndian.PutUint32(out[4:8], uint32(len(b.Base)))
	binary.BigEndian.PutUint32(out[8:12], uint32(len(b.Atlas)))
	binary.BigEndian.PutUint32(out[12:16], uint32(len(meta)))
	off := packHeader
	copy(out[off:], b.Base)
	off += len(b.Base)
	copy(out[off:], b.Atlas)
	off += len(b.Atlas)
	copy(out[off:], meta)
	return out, nil
}

// Unpack 校验并拆开下发格式。会校验长度上限和底帧/图集的 SHA-256。
func Unpack(data []byte) (*frame.Bundle, error) {
	if len(data) < packHeader || string(data[:4]) != packMagic {
		return nil, errors.New("frames: 帧包 magic 不对")
	}
	bl := int(binary.BigEndian.Uint32(data[4:8]))
	al := int(binary.BigEndian.Uint32(data[8:12]))
	ml := int(binary.BigEndian.Uint32(data[12:16]))
	if bl <= 0 || al <= 0 || ml <= 0 ||
		bl > maxPartBytes || al > maxPartBytes || ml > maxPartBytes {
		return nil, errors.New("frames: 帧包长度非法")
	}
	total := packHeader + bl + al + ml
	if total != len(data) || total > MaxPackBytes {
		return nil, errors.New("frames: 帧包长度不一致或超过设备槽上限")
	}
	base := data[packHeader : packHeader+bl]
	atlas := data[packHeader+bl : packHeader+bl+al]
	metaRaw := data[packHeader+bl+al:]

	meta, err := frame.ParseMeta(metaRaw)
	if err != nil {
		return nil, fmt.Errorf("frames: meta 不是合法 JSON：%w", err)
	}
	bs := sha256.Sum256(base)
	if meta.Frame != hex.EncodeToString(bs[:]) {
		return nil, errors.New("frames: 底帧 SHA-256 不匹配")
	}
	as := sha256.Sum256(atlas)
	if meta.Atlas.SHA256 != hex.EncodeToString(as[:]) || meta.Atlas.Size != len(atlas) {
		return nil, errors.New("frames: 图集 SHA-256/长度不匹配")
	}
	return &frame.Bundle{
		Base:  append([]byte(nil), base...),
		Meta:  *meta,
		Atlas: append([]byte(nil), atlas...),
	}, nil
}

func encodeCache(c *cached) []byte {
	buf := make([]byte, cacheHeader+len(c.data))
	copy(buf[:4], cacheMagic)
	binary.BigEndian.PutUint64(buf[4:12], c.ver)
	binary.BigEndian.PutUint64(buf[12:20], uint64(c.at))
	binary.BigEndian.PutUint64(buf[20:28], uint64(c.received))
	used := c.used
	if used == 0 {
		used = c.at
	}
	binary.BigEndian.PutUint64(buf[28:36], uint64(used))
	binary.BigEndian.PutUint32(buf[36:40], uint32(len(c.data)))
	copy(buf[cacheHeader:], c.data)
	return buf
}

func decodeCache(raw []byte) (*cached, error) {
	c, err := decodeHeader(raw)
	if err != nil {
		return nil, err
	}
	hdr := cacheHeader
	if string(raw[:4]) == cacheMagic2 {
		hdr = 32
	}
	if int(c.size) != len(raw)-hdr {
		return nil, errors.New("缓存长度非法")
	}
	data := append([]byte(nil), raw[hdr:]...)
	if _, err := Unpack(data); err != nil {
		return nil, err
	}
	c.data = data
	return c, nil
}

// decodeHeader 只解析缓存头，不复制/校验帧包本身。
func decodeHeader(raw []byte) (*cached, error) {
	if len(raw) < 32 {
		return nil, errors.New("缓存头太短")
	}
	magic := string(raw[:4])
	hdr := cacheHeader
	switch magic {
	case cacheMagic:
	case cacheMagic2:
		hdr = 32
	default:
		return nil, errors.New("缓存 magic 不对")
	}
	if len(raw) < hdr {
		return nil, errors.New("缓存头太短")
	}
	ver := binary.BigEndian.Uint64(raw[4:12])
	at := int64(binary.BigEndian.Uint64(raw[12:20]))
	received := int64(binary.BigEndian.Uint64(raw[20:28]))
	used := at
	var n uint32
	if magic == cacheMagic {
		used = int64(binary.BigEndian.Uint64(raw[28:36]))
		n = binary.BigEndian.Uint32(raw[36:40])
	} else {
		n = binary.BigEndian.Uint32(raw[28:32])
	}
	if ver == 0 || at <= 0 || n == 0 || int(n) > MaxPackBytes {
		return nil, errors.New("缓存长度/版本非法")
	}
	if used <= 0 {
		used = at
	}
	return &cached{ver: ver, at: at, received: received, used: used, size: int64(n)}, nil
}

// readCacheHeader 只读文件头，启动加载用；不把帧包读进内存。
func readCacheHeader(path string) (*cached, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}
	rest := cacheHeader - 4
	if string(head) == cacheMagic2 {
		rest = 32 - 4
	}
	buf := make([]byte, 4+rest)
	copy(buf, head)
	if _, err := io.ReadFull(f, buf[4:]); err != nil {
		return nil, err
	}
	c, err := decodeHeader(buf)
	if err != nil {
		return nil, err
	}
	hdr := int64(cacheHeader)
	if string(head) == cacheMagic2 {
		hdr = 32
	}
	if int64(c.size) != info.Size()-hdr {
		return nil, errors.New("缓存文件长度与头不一致")
	}
	return c, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
