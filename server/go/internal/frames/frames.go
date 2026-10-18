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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/metrics"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/theme"
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
	// ErrNoTheme 表示请求的主题这台服务端不认识。
	ErrNoTheme = errors.New("frames: unknown theme")

	errNoDevices = errors.New("frames: no devices")
	// errNoThemeData 表示这个非主主题那一路还没有数据，这一轮不出它的帧。
	errNoThemeData = errors.New("frames: theme has no data")
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
	// UserTZ 返回用户级时区设置；空串表示未设置。
	UserTZ(userID string) (string, error)
}

// Publisher 把帧版本通知推给一台设备。
type Publisher interface {
	PublishFrame(dev string, payload []byte) error
}

// Builder 把某个主题的快照渲染成帧包。单测里换成假实现，避免真的跑渲染器。
type Builder interface {
	Build(theme.Theme, source.Snapshot) (*frame.Bundle, error)
}

// ThemeBuilder 是生产用的 Builder：直接走 frame.Build。
type ThemeBuilder struct{}

func (ThemeBuilder) Build(th theme.Theme, s source.Snapshot) (*frame.Bundle, error) {
	return frame.Build(th, s)
}

// Themed 把一个主题和它吃的那一路数据绑在一起。
//
// 顺序就是播放列表的优先级：第一个是主主题，设备装不下全部时从前往后留，
// 老固件也只认它那一个版本号。
type Themed struct {
	Theme  theme.Theme
	Source source.Source
}

// packKey 是帧包的取用键：一个用户的一个主题一份帧包。
//
// 落盘是 <root>/frames/<user>/<theme>.cache——按用户分目录而不是把主题
// 拼进目录名，这样解绑时删掉整个用户目录就能连带清掉它的全部主题。
type packKey struct {
	user  string
	theme string
}

func (k packKey) String() string { return k.user + "/" + k.theme }

type Options struct {
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
	// Dwell 是播放列表里每个主题的停留时长。只有一个主题时没有意义。
	Dwell time.Duration
	Now   func() time.Time
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

// memNode 记录 LRU 节点入账时的帧包大小，避免替换/命中时 memBytes 漂移。
type memNode struct {
	k    packKey
	size int64
}

// evicted 是磁盘淘汰的用户，用于在锁外删目录并写版本墓碑。
type evicted struct {
	k   packKey
	ver uint64
}

// Service 维护全部用户的当前帧。
type Service struct {
	root    string
	store   Store
	themes  []Themed
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
	cache map[packKey]*cached

	// 常驻内存 LRU：cache 里的 data 只有最近用过的用户才有。
	memLRU    *list.List // 值 = *memNode；front 最近使用
	memIndex  map[packKey]*list.Element
	memBytes  int64
	diskBytes int64

	// 启动补帧队列：限速逐个 MarkDirty，避免重连时一次性渲染所有用户。
	backfillMu  sync.Mutex
	backfillQ   []string
	backfillSet map[string]struct{}
}

func New(root string, st Store, themes []Themed, pub Publisher, b Builder, opt Options) (*Service, error) {
	if root == "" {
		return nil, errors.New("frames: root 为空")
	}
	if st == nil || len(themes) == 0 || pub == nil || b == nil {
		return nil, errors.New("frames: 缺依赖")
	}
	for i, t := range themes {
		if t.Theme == nil || t.Source == nil {
			return nil, fmt.Errorf("frames: 第 %d 个主题缺主题或数据源", i)
		}
		if !safeID(t.Theme.ID()) {
			return nil, fmt.Errorf("frames: 主题 ID %q 不能用作文件名", t.Theme.ID())
		}
	}
	opt.Dwell = theme.ClampDwell(opt.Dwell)
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
		root: root, store: st, themes: themes, pub: pub, builder: b, opt: opt,
		ctx: ctx, cancel: cancel,
		workers:     map[string]*worker{},
		renderSem:   make(chan struct{}, opt.RenderConcurrency),
		cache:       map[packKey]*cached{},
		memLRU:      list.New(),
		memIndex:    map[packKey]*list.Element{},
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
func (s *Service) cacheFile(k packKey) string {
	return filepath.Join(s.userDir(k.user), k.theme+".cache")
}

// safeUserID 只挡路径分隔符和越界长度；store 生成的是 32 位十六进制。
func safeUserID(id string) bool { return safeID(id) }

// safeID 是能安全拼进路径的标识符：用户 ID 和主题 ID 共用这一套。
func safeID(id string) bool {
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
		files, err := os.ReadDir(filepath.Join(s.dir(), e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".cache") {
				continue // 还包括 commit 留下的 .frame-*.tmp
			}
			id := strings.TrimSuffix(name, ".cache")
			if !safeID(id) {
				continue
			}
			k := packKey{user: e.Name(), theme: id}
			c, err := readCacheHeader(s.cacheFile(k))
			if err != nil {
				slog.Warn("帧缓存损坏，忽略", "pack", k, "err", err)
				continue
			}
			s.cache[k] = c
			s.diskBytes += c.size + cacheHeader
		}
	}
	// 启动时磁盘已超限：按 used 淘汰。
	s.mu.Lock()
	victims := s.evictDiskLocked(packKey{}, s.opt.CacheMaxBytes)
	s.mu.Unlock()
	s.finishEvictions(victims)
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
	now := s.opt.Now()

	// 每个主题吃自己那一路数据，各渲一个帧包。一个主题挂了不影响别的：
	// 它保留上一张好帧，播放列表里照样有它。全挂了才算这次重建失败。
	var firstErr error
	built := 0
	for i, t := range s.themes {
		if err := s.rebuildTheme(userID, t, now, i == 0); err != nil {
			// 「这一路还没数据」不是错误，是正常的「这个主题先不上屏」。
			if firstErr == nil && !errors.Is(err, errNoThemeData) {
				firstErr = err
			}
			continue
		}
		built++
	}
	// 有主题成功就算这一轮成功：失败的那个保留上一张好帧，照样在列表里。
	if built == 0 {
		return firstErr
	}
	// 通知放在全部主题渲完之后发一次：播放列表是一个整体，中途发会让
	// 设备按只有一半主题的列表去取包。
	s.notify(userID)
	return nil
}

// primary 为真时无论有没有数据都出帧：新配对的设备总得有东西可显示
// （那张「等待上报」的帧）。非主主题要等它那一路真有数据才进轮播——
// 否则只报了一路的用户会看到一块空白看板在那轮流上屏。
func (s *Service) rebuildTheme(userID string, t Themed, now time.Time, primary bool) error {
	snap, err := t.Source.Snapshot(s.ctx, userID, now)
	if err != nil {
		return fmt.Errorf("frames: 取 %s 数据：%w", t.Source.ID(), err)
	}
	if !primary && !snap.HasData() {
		return errNoThemeData
	}
	bun, err := s.builder.Build(t.Theme, snap)
	if err != nil {
		slog.Warn("渲染帧失败，保留旧帧", "user", userID, "theme", t.Theme.ID(), "err", err)
		return fmt.Errorf("frames: 渲染 %s：%w", t.Theme.ID(), err)
	}
	data, err := Pack(bun)
	if err != nil {
		slog.Warn("打包帧失败，保留旧帧", "user", userID, "theme", t.Theme.ID(), "err", err)
		return fmt.Errorf("frames: 打包 %s：%w", t.Theme.ID(), err)
	}
	k := packKey{user: userID, theme: t.Theme.ID()}
	return s.commit(k, data, now, snap.LastUpdate, snap.HasData())
}

// commit 写临时文件（含 fsync）在提交锁内、读锁外；内存里只做 rename 和替换。
// 比当前帧更旧的快照直接丢弃，避免并发上报时旧数据覆盖新数据。
func (s *Service) commit(k packKey, data []byte, now time.Time, received time.Time, hasUsage bool) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	s.mu.RLock()
	cur := s.cache[k]
	s.mu.RUnlock()
	if cur != nil && hasUsage && received.Unix() < cur.received {
		return nil
	}
	// 渲染期间可能刚好被解绑：名下没设备就不要再把缓存写回来。
	devs, err := s.store.DevicesOfUser(k.user)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return errNoDevices
	}

	ver := uint64(1)
	if cur != nil {
		ver = cur.ver + 1
	} else if gone, ok := s.readGoneVer(k); ok {
		// 帧被磁盘 LRU 淘汰过：从墓碑接着递增，绝不回到 1。
		// 固件拿版本号做等值比较，回退会让它误以为手里已是最新帧。
		ver = gone + 1
	}
	rec := int64(0)
	if hasUsage {
		rec = received.Unix()
	}
	c := &cached{data: data, ver: ver, at: now.Unix(), received: rec}

	tmp, err := s.writeTemp(k, c)
	if err != nil {
		return fmt.Errorf("frames: 写缓存：%w", err)
	}

	s.mu.Lock()
	if err := os.Rename(tmp, s.cacheFile(k)); err != nil {
		s.mu.Unlock()
		os.Remove(tmp)
		return err
	}
	c.used = now.Unix()
	c.size = int64(len(data))
	s.cache[k] = c
	if cur != nil {
		s.diskBytes += c.size - cur.size
	} else {
		s.diskBytes += c.size + cacheHeader
	}
	s.touchMemLocked(k, c)
	victims := s.evictDiskLocked(k, s.opt.CacheMaxBytes)
	s.mu.Unlock()

	// 这次提交成功后墓碑就过时了（版本已经重新建立在它之上）。
	os.Remove(s.goneFile(k))
	s.finishEvictions(victims)
	if err := syncDir(s.userDir(k.user)); err != nil {
		slog.Warn("同步帧缓存目录失败", "user", k.user, "err", err)
	}
	return nil
}

// touchMemLocked 把用户帧包放进常驻内存 LRU，并淘汰最久没被取用的。
// 要求持有 s.mu。
func (s *Service) touchMemLocked(k packKey, c *cached) {
	if c.data == nil {
		return
	}
	if e, ok := s.memIndex[k]; ok {
		n := e.Value.(*memNode)
		if n.size != c.size {
			s.memBytes += c.size - n.size
			n.size = c.size
		}
		s.memLRU.MoveToFront(e)
		return
	}
	n := &memNode{k: k, size: c.size}
	s.memIndex[k] = s.memLRU.PushFront(n)
	s.memBytes += c.size
	for s.memBytes > s.opt.MemoryMaxBytes && s.memLRU.Len() > 1 {
		back := s.memLRU.Back()
		node := back.Value.(*memNode)
		s.memLRU.Remove(back)
		delete(s.memIndex, node.k)
		s.memBytes -= node.size
		if old, ok := s.cache[node.k]; ok {
			old.data = nil
		}
	}
}

// evictDiskLocked 按 used 从旧到新删除，直到磁盘总量回到上限内。
// 返回需要删除目录的用户及其版本（版本写墓碑，保证不倒退）；
// 要求持有 s.mu。
func (s *Service) evictDiskLocked(keep packKey, max int64) []evicted {
	var victims []evicted
	for s.diskBytes > max {
		var oldestK packKey
		var oldest int64
		found := false
		for k, c := range s.cache {
			if k == keep {
				continue
			}
			if !found || c.used < oldest {
				oldestK, oldest, found = k, c.used, true
			}
		}
		if !found {
			break
		}
		old := s.cache[oldestK]
		delete(s.cache, oldestK)
		s.diskBytes -= old.size + cacheHeader
		if e, ok := s.memIndex[oldestK]; ok {
			node := e.Value.(*memNode)
			s.memLRU.Remove(e)
			delete(s.memIndex, oldestK)
			s.memBytes -= node.size
		}
		victims = append(victims, evicted{k: oldestK, ver: old.ver})
	}
	return victims
}

// finishEvictions 在锁外删目录并写版本墓碑。
func (s *Service) finishEvictions(victims []evicted) {
	if len(victims) == 0 {
		return
	}
	metrics.IncFrameCacheEviction(len(victims))
	for _, v := range victims {
		if err := os.MkdirAll(s.goneDir(), 0o700); err != nil {
			slog.Warn("创建版本墓碑目录失败", "err", err)
		} else if err := os.WriteFile(s.goneFile(v.k),
			[]byte(strconv.FormatUint(v.ver, 10)), 0o600); err != nil {
			slog.Warn("写版本墓碑失败", "pack", v.k, "err", err)
		}
		// 只删这一个主题的帧包，不能动整个用户目录：同一个用户的别的
		// 主题还在用，而且 drop 才是「这个用户整个没了」的那条路。
		if err := os.Remove(s.cacheFile(v.k)); err != nil && !os.IsNotExist(err) {
			slog.Warn("淘汰帧缓存失败", "pack", v.k, "err", err)
		}
		// 这个用户的最后一个主题也被淘汰了就把空目录收掉，别留一地空壳。
		// 目录还有别的主题时 Remove 自然失败，正好是想要的语义。
		// 与 commit 不会打架：两边都在 commitMu 里。
		os.Remove(s.userDir(v.k.user))
	}
}

func (s *Service) goneDir() string { return filepath.Join(s.dir(), ".gone") }

func (s *Service) goneFile(k packKey) string {
	return filepath.Join(s.goneDir(), k.user+"."+k.theme)
}

// readGoneVer 读被淘汰帧包的版本墓碑；没有墓碑表示从未淘汰过。
func (s *Service) readGoneVer(k packKey) (uint64, bool) {
	raw, err := os.ReadFile(s.goneFile(k))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

func (s *Service) writeTemp(k packKey, c *cached) (string, error) {
	dir := s.userDir(k.user)
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

// Playlist 是这个用户当前的轮播表，按主题的注册顺序（即优先级）排。
//
// 只收已经有帧包的主题：没渲出来的主题进了列表，设备会去取一个 404。
// 不按设备能力裁剪长度——服务端不知道对面那块板的 flash 有多大，
// 设备自己从前往后留下装得下的那些。见 theme.Playlist 的说明。
func (s *Service) Playlist(userID string) theme.Playlist {
	s.mu.RLock()
	defer s.mu.RUnlock()
	secs := int(s.opt.Dwell / time.Second)
	pl := make(theme.Playlist, 0, len(s.themes))
	for _, t := range s.themes {
		c, ok := s.cache[packKey{user: userID, theme: t.Theme.ID()}]
		if !ok {
			continue
		}
		pl = append(pl, theme.Slot{ID: t.Theme.ID(), Ver: c.ver, Secs: secs})
	}
	return pl
}

// notify 把当前播放列表发给该用户名下所有设备。失败不影响帧本身，
// 设备重连订阅时会由 Resend 补上。
func (s *Service) notify(userID string) {
	pl := s.Playlist(userID)
	if len(pl) == 0 {
		return
	}
	devs, err := s.store.DevicesOfUser(userID)
	if err != nil {
		slog.Warn("列设备失败，跳过帧通知", "user", userID, "err", err)
		return
	}
	payload := playlistPayload(pl)
	for _, dev := range devs {
		if err := s.pub.PublishFrame(dev, payload); err != nil {
			slog.Warn("推送播放列表失败", "dev", dev, "err", err)
		}
	}
}

// playlistPayload 是 frame 主题的消息体。
//
// ver 是主主题的版本，单独留着给老固件：它只认 {"ver":N}，不认 pl。
// 新固件读 pl，忽略 ver。两个字段必须一致地描述主主题，否则老固件会
// 取到一个它以为是最新、实际是别的主题的包。
func playlistPayload(pl theme.Playlist) []byte {
	primary, _ := pl.Primary()
	b, _ := json.Marshal(struct {
		Ver uint64         `json:"ver"`
		PL  theme.Playlist `json:"pl"`
	}{primary.Ver, pl})
	return b
}

func (s *Service) cachedVersion(k packKey) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cache[k]
	if !ok {
		return 0, false
	}
	return c.ver, true
}

// primaryTheme 是主主题的 ID：老固件不带 theme 参数时给它这一个。
func (s *Service) primaryTheme() string { return s.themes[0].Theme.ID() }

// ETag 是取帧响应的实体标签。必须带主题名：各主题的版本各自递增，
// 两个主题都到 v7 时，只用版本号会让切换主题的设备收到假 304。
// themeID 为空表示主主题（老固件的请求）。
func (s *Service) ETag(themeID string, ver uint64) string {
	if themeID == "" {
		themeID = s.primaryTheme()
	}
	return themeID + "-" + strconv.FormatUint(ver, 10)
}

// Current 返回用户的当前帧包与版本。返回的字节是共享缓存，调用方不要改。
//
// 帧包不常驻内存：不在 LRU 里时从磁盘读一次，同时更新 used 供淘汰。
//
// 读文件时不持锁。期间 commit 可能把文件换成 v2，而 v2 的 data 又立刻被
// 内存 LRU 淘汰——如果不比对文件里的版本，就会把 v1 的字节挂到 v2 名下。
// 所以 readPackFile 连版本一起返回，和内存里的 c.ver 不一致就丢弃重读。
func (s *Service) Current(userID, themeID string) (data []byte, ver uint64, ok bool) {
	if themeID == "" {
		themeID = s.primaryTheme() // 老调用方（admin、运维）不关心主题
	}
	k := packKey{user: userID, theme: themeID}
	for attempt := 0; attempt < 3; attempt++ {
		s.mu.Lock()
		c, ok := s.cache[k]
		if !ok {
			s.mu.Unlock()
			return nil, 0, false
		}
		if c.data != nil {
			if e, ok := s.memIndex[k]; ok {
				s.memLRU.MoveToFront(e)
			}
			c.used = s.opt.Now().Unix()
			data, ver := c.data, c.ver
			s.mu.Unlock()
			return data, ver, true
		}
		s.mu.Unlock()

		got, fileVer, err := readPackFile(s.cacheFile(k))
		if err != nil {
			slog.Warn("读帧缓存失败", "pack", k, "err", err)
			return nil, 0, false
		}

		s.mu.Lock()
		c, ok = s.cache[k]
		if !ok {
			s.mu.Unlock()
			return nil, 0, false
		}
		if c.ver != fileVer {
			// 文件已经被更新的提交换掉了；重读一次。
			s.mu.Unlock()
			continue
		}
		if c.data == nil {
			c.data = got
			c.size = int64(len(got))
			s.touchMemLocked(k, c)
		}
		if e, ok := s.memIndex[k]; ok {
			s.memLRU.MoveToFront(e)
		}
		c.used = s.opt.Now().Unix()
		data, ver := c.data, c.ver
		s.mu.Unlock()
		return data, ver, true
	}
	return nil, 0, false
}

// readPackFile 读取并校验磁盘缓存，返回帧包字节与文件里的版本。
func readPackFile(path string) ([]byte, uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	c, err := decodeCache(raw)
	if err != nil {
		return nil, 0, err
	}
	return c.data, c.ver, nil
}

// CurrentForKey 按令牌自己的 KeyID 判权：未配对、已吊销、角色不对、
// dev 不符或没有 user_id 都返回 ErrNotPaired。不要按 dev 反查主人。
// themeID 为空表示老固件没带 theme 参数，给它主主题。
func (s *Service) CurrentForKey(keyID, dev, themeID string) ([]byte, uint64, error) {
	key, found, err := s.store.KeyByID(keyID)
	if err != nil {
		return nil, 0, err
	}
	if !found || key.Disabled || key.Pending || key.Role != "device" ||
		key.Dev != dev || key.UserID == "" {
		return nil, 0, ErrNotPaired
	}
	if themeID == "" {
		themeID = s.primaryTheme()
	} else if !s.hasTheme(themeID) {
		return nil, 0, ErrNoTheme
	}
	data, ver, ok := s.Current(key.UserID, themeID)
	if !ok {
		// 懒加载：设备第一次取帧时才为这个用户渲染，不在启动时全量补帧。
		s.MarkDirty(key.UserID)
		return nil, 0, ErrNoFrame
	}
	return data, ver, nil
}

// hasAnyPack 报告这个用户有没有任何一个主题的帧包。
func (s *Service) hasAnyPack(userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.themes {
		if _, ok := s.cache[packKey{user: userID, theme: t.Theme.ID()}]; ok {
			return true
		}
	}
	return false
}

func (s *Service) hasTheme(id string) bool {
	for _, t := range s.themes {
		if t.Theme.ID() == id {
			return true
		}
	}
	return false
}

// Resend 在设备订阅 frame 主题时补发当前版本。
func (s *Service) Resend(dev string) {
	owner, bound, err := s.store.OwnerOfDevice(dev)
	if err != nil || !bound {
		return
	}
	pl := s.Playlist(owner)
	if len(pl) == 0 {
		// 设备订阅了 frame 但还没有帧：懒加载，不等启动补帧队列。
		s.MarkDirty(owner)
		return
	}
	if err := s.pub.PublishFrame(dev, playlistPayload(pl)); err != nil {
		slog.Warn("补发播放列表失败", "dev", dev, "err", err)
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

	// 这个用户整个没了：它的每一个主题都要清掉。
	var gone []packKey
	s.mu.Lock()
	for k, c := range s.cache {
		if k.user != userID {
			continue
		}
		s.diskBytes -= c.size + cacheHeader
		delete(s.cache, k)
		if e, ok := s.memIndex[k]; ok {
			node := e.Value.(*memNode)
			s.memLRU.Remove(e)
			delete(s.memIndex, k)
			s.memBytes -= node.size
		}
		gone = append(gone, k)
	}
	s.mu.Unlock()

	// 解绑/删除用户后墓碑也没有意义了。
	for _, k := range gone {
		os.Remove(s.goneFile(k))
	}

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
	// 按用户遍历而不是按帧包：MarkDirty 和 drop 都是用户级的，
	// 一个用户的任何一个主题该重渲，整个用户重渲一轮就行。
	s.mu.RLock()
	cachedUsers := make(map[string]struct{}, len(s.cache))
	for k := range s.cache {
		cachedUsers[k.user] = struct{}{}
	}
	s.mu.RUnlock()

	for id := range cachedUsers {
		devs, err := s.store.DevicesOfUser(id)
		if err != nil {
			continue
		}
		if len(devs) == 0 {
			s.drop(id)
			continue
		}
		if s.userNeedsRebuild(id, now) {
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
		if s.hasAnyPack(u.ID) {
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
// userNeedsRebuild 判断这个用户要不要重渲一轮：跨天，或者任何一个主题的
// 数据已经旧到该把「更新 X 分前」和告警色刷一遍了。
//
// 每个主题各问自己那一路数据源要时区和上报时刻。这里一分钟跑一次、
// 每个主题各一次，所以 Source 的实现必须是便宜的本地读。
func (s *Service) userNeedsRebuild(userID string, now time.Time) bool {
	for _, t := range s.themes {
		s.mu.RLock()
		c := s.cache[packKey{user: userID, theme: t.Theme.ID()}]
		s.mu.RUnlock()
		if c == nil {
			continue
		}
		snap, err := t.Source.Snapshot(s.ctx, userID, now)
		if err != nil {
			continue
		}
		loc := snap.Now.Location()
		built := time.Unix(c.at, 0).In(loc)
		if built.Format("2006-01-02") != now.In(loc).Format("2006-01-02") {
			return true // 跨天：日期/星期变了
		}
		// 超过 15 分钟没上报：重渲一次，让「更新 X 分前」和告警色跟上；
		// 之后每 15 分钟一次（帧的 at 会更新）。
		if snap.HasData() && now.Sub(snap.LastUpdate) >= s.opt.StaleAfter &&
			now.Sub(time.Unix(c.at, 0)) >= s.opt.StaleAfter {
			return true
		}
	}
	return false
}

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
		if s.hasAnyPack(id) {
			continue
		}
		s.MarkDirty(id)
	}
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
