// Package frames 维护每个用户的当前帧包并负责下发。
//
// 渲染是重活（T13a 一帧约 1 秒），所以只在这些时刻重建：
//   - agent 上报用量成功之后（httpapi 触发）
//   - 设备配对 / 解绑之后
//   - 每天 0 点（帧里的日期会变；服务重启时也会补一次跨天检查）
//
// 帧包同时落盘到 <root>/frames/<userID>/frame.cache，进程重启直接
// 从磁盘加载，不重新渲染。渲染失败时保留上一份好帧。
package frames

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/frame"
	"github.com/dayuer/stickbox/server/go/internal/render"
	"github.com/dayuer/stickbox/server/go/internal/source"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

const (
	// 下发给设备的帧包：magic + 三段长度 + base + atlas + meta。
	packMagic  = "SBF1"
	packHeader = 16

	// 落盘缓存：magic + version + built_at + packLen + pack。
	cacheMagic  = "SBC1"
	cacheHeader = 24

	maxPartBytes   = 1 << 20
	maxBundleBytes = 2 << 20
)

var (
	// ErrNotPaired 表示设备密钥还没完成配对。
	ErrNotPaired = errors.New("frames: device not paired")
	// ErrNoFrame 表示设备已配对，但还没有可下发的帧。
	ErrNoFrame = errors.New("frames: frame not ready")
)

// Store 是帧服务对 store 的窄依赖。
type Store interface {
	DevicesOfUser(userID string) ([]string, error)
	OwnerOfDevice(dev string) (string, bool, error)
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

type cached struct {
	data []byte // 已打包的下发格式
	ver  uint64
	at   int64 // 构建时刻（Unix 秒）
}

// Service 维护全部用户的当前帧。
type Service struct {
	root    string
	store   Store
	usage   Usage
	pub     Publisher
	builder Builder
	now     func() time.Time

	mu    sync.RWMutex
	cache map[string]*cached
}

func New(root string, st Store, us Usage, pub Publisher, b Builder, now func() time.Time) (*Service, error) {
	if root == "" {
		return nil, errors.New("frames: root 为空")
	}
	if st == nil || us == nil || pub == nil || b == nil {
		return nil, errors.New("frames: 缺依赖")
	}
	if now == nil {
		now = time.Now
	}
	s := &Service{
		root: root, store: st, usage: us, pub: pub, builder: b, now: now,
		cache: map[string]*cached{},
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("frames: 创建缓存目录：%w", err)
	}
	s.load()
	return s, nil
}

func (s *Service) dir() string { return filepath.Join(s.root, "frames") }

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
		raw, err := os.ReadFile(filepath.Join(s.dir(), e.Name(), "frame.cache"))
		if err != nil {
			slog.Warn("读帧缓存失败", "user", e.Name(), "err", err)
			continue
		}
		c, err := decodeCache(raw)
		if err != nil {
			slog.Warn("帧缓存损坏，忽略", "user", e.Name(), "err", err)
			continue
		}
		s.cache[e.Name()] = c
	}
}

// Rebuild 渲染并替换一个用户的帧，成功后版本加一并通知名下设备。
// 失败时不动旧帧。
func (s *Service) Rebuild(userID string) error {
	if !safeUserID(userID) {
		return fmt.Errorf("frames: 非法 userID %q", userID)
	}
	now := s.now()

	rep, received, ok, err := s.usage.Latest(userID)
	if err != nil {
		slog.Warn("读用量失败，保留旧帧", "user", userID, "err", err)
		return fmt.Errorf("frames: 读用量：%w", err)
	}
	var snap source.Snapshot
	if ok {
		snap, err = source.Claude{Report: rep, ReceivedAt: received}.Snapshot(context.Background(), now)
	} else {
		// 刚配对、agent 还没上报过：也要出一张「等待上报」的帧。
		snap, err = source.Claude{}.Snapshot(context.Background(), now)
	}
	if err != nil {
		slog.Warn("生成快照失败，保留旧帧", "user", userID, "err", err)
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

	s.mu.Lock()
	ver := uint64(1)
	if old := s.cache[userID]; old != nil {
		ver = old.ver + 1
	}
	c := &cached{data: data, ver: ver, at: now.Unix()}
	if err := s.persist(userID, c); err != nil {
		s.mu.Unlock()
		slog.Warn("帧落盘失败，保留旧帧", "user", userID, "err", err)
		return fmt.Errorf("frames: 落盘：%w", err)
	}
	s.cache[userID] = c
	s.mu.Unlock()

	s.notify(userID, ver)
	return nil
}

func (s *Service) persist(userID string, c *cached) error {
	dir := filepath.Join(s.dir(), userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	buf := make([]byte, cacheHeader+len(c.data))
	copy(buf[:4], cacheMagic)
	binary.BigEndian.PutUint64(buf[4:12], c.ver)
	binary.BigEndian.PutUint64(buf[12:20], uint64(c.at))
	binary.BigEndian.PutUint32(buf[20:24], uint32(len(c.data)))
	copy(buf[cacheHeader:], c.data)

	tmp, err := os.CreateTemp(dir, ".frame-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, "frame.cache"))
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
func (s *Service) Current(userID string) (data []byte, ver uint64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cache[userID]
	if !ok {
		return nil, 0, false
	}
	return c.data, c.ver, true
}

// CurrentForDevice 按设备令牌取帧：未配对返回 ErrNotPaired，
// 配对但还没帧返回 ErrNoFrame。
func (s *Service) CurrentForDevice(dev string) (data []byte, ver uint64, err error) {
	owner, bound, err := s.store.OwnerOfDevice(dev)
	if err != nil {
		return nil, 0, err
	}
	if !bound {
		return nil, 0, ErrNotPaired
	}
	data, ver, ok := s.Current(owner)
	if !ok {
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
		return
	}
	if err := s.pub.PublishFrame(dev, versionPayload(ver)); err != nil {
		slog.Warn("补发帧版本失败", "dev", dev, "err", err)
	}
}

// RunDaily 每天 0 点重渲染（帧里的日期会变）。启动时先补一次跨天检查。
func (s *Service) RunDaily(ctx context.Context) {
	s.rebuildStale()
	for {
		now := s.now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
			AddDate(0, 0, 1)
		d := next.Sub(now)
		if d <= 0 {
			d = time.Minute
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			s.rebuildStale()
		}
	}
}

// rebuildStale 重建所有「构建日期不是今天」的帧。
func (s *Service) rebuildStale() {
	now := s.now()
	today := now.Format("2006-01-02")

	s.mu.RLock()
	ids := make([]string, 0, len(s.cache))
	for id, c := range s.cache {
		if time.Unix(c.at, 0).In(now.Location()).Format("2006-01-02") != today {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()

	for _, id := range ids {
		if err := s.Rebuild(id); err != nil {
			slog.Warn("跨天重建帧失败", "user", id, "err", err)
		}
	}
}

// Pack 生成下发格式：
//
//	"SBF1" | baseLen uint32 | atlasLen uint32 | metaLen uint32 | base | atlas | meta
//
// 三个长度都是大端。
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
	if total > maxBundleBytes {
		return nil, errors.New("frames: 帧包过大")
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

// Unpack 校验并拆开下发格式。会校验底帧/图集的 SHA-256。
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
	if total != len(data) || total > maxBundleBytes {
		return nil, errors.New("frames: 帧包长度不一致")
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

func decodeCache(raw []byte) (*cached, error) {
	if len(raw) < cacheHeader || string(raw[:4]) != cacheMagic {
		return nil, errors.New("缓存 magic 不对")
	}
	ver := binary.BigEndian.Uint64(raw[4:12])
	at := int64(binary.BigEndian.Uint64(raw[12:20]))
	n := binary.BigEndian.Uint32(raw[20:24])
	if ver == 0 || at <= 0 || n == 0 || int(n) > maxBundleBytes ||
		int(n) != len(raw)-cacheHeader {
		return nil, errors.New("缓存长度/版本非法")
	}
	data := append([]byte(nil), raw[cacheHeader:]...)
	if _, err := Unpack(data); err != nil {
		return nil, err
	}
	return &cached{data: data, ver: ver, at: at}, nil
}
