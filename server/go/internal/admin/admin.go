// Package admin 提供只绑定回环地址的设备诊断 HTTP 接口。
//
// stickboxd device show <dev> / user tz 在服务器本机调用它。除
// POST /user-tz 外接口只读；密钥材料（hash）永远不进响应，配置层也
// 强制只监听回环地址。
package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/broker"
	"github.com/dayuer/stickbox/server/go/internal/command"
	"github.com/dayuer/stickbox/server/go/internal/store"
	"github.com/dayuer/stickbox/server/go/internal/tz"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

type Store interface {
	KeysOfDevice(dev string) ([]store.Key, error)
	OwnerOfDevice(dev string) (string, bool, error)
}

type Usage interface {
	Latest(userID string) (usage.Report, time.Time, bool, error)
}

type Frames interface {
	// themeID 为空表示主主题。admin 只关心「有没有帧、版本多少」。
	Current(userID, themeID string) (data []byte, ver uint64, ok bool)
}

// Runtime 是运行中的 broker/tracker 提供的易失状态。
type Runtime interface {
	RecentAuth(dev string, n int) []broker.AuthEvent
	ConnectedKeys(dev string) []string
	Pending(dev string) []command.PendingInfo
	RegisterQuota(dev string) (left, limit int, reset time.Time)
}

// TZApplier 由运行中的进程提供：写库、同步重渲并通知设备。
// CLI 跑在另一个进程，只有通过它才能让修改立刻生效。
type TZApplier interface {
	ApplyUserTZ(userID, tz string) error
}

// PhotoStatus 提供设备最近一次 status.photo。
type PhotoStatus interface {
	Get(dev string) (st PhotoStatusInfo, ok bool)
}

// PhotoStatusInfo 是 device show 的 photo 投影。
type PhotoStatusInfo struct {
	Cap      bool
	Ready    bool
	PLV      int64
	Have     int
	Bad      []string
	FreeKB   int64
	State    string
	InitDone string
}

type Handler struct {
	st    Store
	us    Usage
	fr    Frames
	rt    Runtime
	tz    TZApplier
	photo PhotoStatus
	now   func() time.Time
}

func NewHandler(st Store, us Usage, fr Frames, rt Runtime) *Handler {
	return &Handler{st: st, us: us, fr: fr, rt: rt, now: time.Now}
}

// SetTZApplier 注入时区写入能力；不注入时 POST /user-tz 返回 503。
func (h *Handler) SetTZApplier(a TZApplier) { h.tz = a }

// SetPhotoStatus 注入设备照片状态（未注入时 device show 不显示这一段）。
func (h *Handler) SetPhotoStatus(p PhotoStatus) { h.photo = p }

type Report struct {
	Dev           string                `json:"dev"`
	Now           time.Time             `json:"now"`
	Bound         bool                  `json:"bound"`
	UserID        string                `json:"user_id,omitempty"`
	Keys          []KeyInfo             `json:"keys"`
	ConnectedKeys []string              `json:"connected_keys"`
	RecentAuth    []broker.AuthEvent    `json:"recent_auth"`
	Pending       []command.PendingInfo `json:"pending"`
	RegisterQuota QuotaInfo             `json:"register_quota"`
	Usage         *UsageInfo            `json:"usage,omitempty"`
	Frame         *FrameInfo            `json:"frame,omitempty"`
	Photo         *PhotoInfo            `json:"photo,omitempty"`
}

type PhotoInfo struct {
	Cap      bool     `json:"cap"`
	Ready    bool     `json:"ready"`
	State    string   `json:"state"`
	PLV      int64    `json:"plv"`
	Have     int      `json:"have"`
	Bad      []string `json:"bad,omitempty"`
	FreeKB   int64    `json:"free_kb"`
	InitDone string   `json:"init_done,omitempty"`
}

type KeyInfo struct {
	KeyID    string `json:"key_id"`
	State    string `json:"state"` // active / pending / disabled
	Role     string `json:"role"`
	UserID   string `json:"user_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"last_seen"`
}

type QuotaInfo struct {
	Remaining int        `json:"remaining"`
	Limit     int        `json:"limit"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

type UsageInfo struct {
	FiveHour   *usage.Window `json:"five_hour,omitempty"`
	SevenDay   *usage.Window `json:"seven_day,omitempty"`
	TZ         string        `json:"tz,omitempty"`
	ReceivedAt time.Time     `json:"received_at"`
}

type FrameInfo struct {
	Version   uint64 `json:"version"`
	Available bool   `json:"available"`
}

const maxRecent = 50

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackRemote(r.RemoteAddr) {
		// 配置已经强制回环监听；这里再挡一层，防止有人把 admin_addr
		// 改到 0.0.0.0 或前面挂了会改写来源地址的代理。
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/user-tz" {
		h.serveUserTZ(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dev := r.URL.Query().Get("dev")
	if !auth.ValidDev(dev) {
		http.Error(w, "bad dev", http.StatusBadRequest)
		return
	}
	n := 10
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxRecent {
		n = maxRecent
	}

	rep, err := h.collect(dev, n)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(rep)
}

// serveUserTZ 是 admin 唯一的写接口：stickboxd user tz 用它让运行中的
// 进程立刻重渲。只接受回环来源（ServeHTTP 已挡），不做鉴权。
func (h *Handler) serveUserTZ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.tz == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		UserID string  `json:"user_id"`
		TZ     *string `json:"tz"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.UserID) == "" || in.TZ == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	value := ""
	if strings.TrimSpace(*in.TZ) != "" {
		v, err := tz.Normalize(*in.TZ)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		value = v
	}
	if err := h.tz.ApplyUserTZ(in.UserID, value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": 1, "tz": value})
}

func (h *Handler) collect(dev string, n int) (Report, error) {
	rep := Report{
		Dev: dev, Now: h.now(),
		Keys:          []KeyInfo{},
		ConnectedKeys: []string{},
		RecentAuth:    []broker.AuthEvent{},
		Pending:       []command.PendingInfo{},
	}
	keys, err := h.st.KeysOfDevice(dev)
	if err != nil {
		return rep, err
	}
	for _, k := range keys {
		rep.Keys = append(rep.Keys, KeyInfo{
			KeyID: k.KeyID, State: keyState(k), Role: k.Role, UserID: k.UserID,
			Name: k.Name, Created: k.Created, LastSeen: k.LastSeen,
		})
	}
	sort.Slice(rep.Keys, func(i, j int) bool {
		ri, rj := stateRank(rep.Keys[i].State), stateRank(rep.Keys[j].State)
		if ri != rj {
			return ri < rj
		}
		return rep.Keys[i].Created > rep.Keys[j].Created
	})

	left, limit, reset := h.rt.RegisterQuota(dev)
	rep.RegisterQuota = QuotaInfo{Remaining: left, Limit: limit}
	if !reset.IsZero() {
		rep.RegisterQuota.ResetAt = &reset
	}
	if keys := h.rt.ConnectedKeys(dev); keys != nil {
		rep.ConnectedKeys = keys
	}
	if auths := h.rt.RecentAuth(dev, n); auths != nil {
		rep.RecentAuth = auths
	}
	if pend := h.rt.Pending(dev); pend != nil {
		rep.Pending = pend
	}

	owner, bound, err := h.st.OwnerOfDevice(dev)
	if err != nil {
		return rep, err
	}
	rep.Bound, rep.UserID = bound, owner
	if !bound || owner == "" {
		return rep, nil
	}
	if r, received, ok, err := h.us.Latest(owner); err == nil && ok {
		rep.Usage = &UsageInfo{
			FiveHour: r.FiveHour, SevenDay: r.SevenDay,
			TZ: r.TZ, ReceivedAt: received,
		}
	}
	if h.fr != nil {
		if _, ver, ok := h.fr.Current(owner, ""); ok {
			rep.Frame = &FrameInfo{Version: ver, Available: true}
		} else {
			rep.Frame = &FrameInfo{Available: false}
		}
	}
	if h.photo != nil {
		if st, ok := h.photo.Get(dev); ok {
			rep.Photo = &PhotoInfo{
				Cap: st.Cap, Ready: st.Ready, State: st.State, PLV: st.PLV,
				Have: st.Have, Bad: st.Bad, FreeKB: st.FreeKB, InitDone: st.InitDone,
			}
		}
	}
	return rep, nil
}

func keyState(k store.Key) string {
	switch {
	case k.Disabled:
		return "disabled"
	case k.Pending:
		return "pending"
	default:
		return "active"
	}
}

func stateRank(s string) int {
	switch s {
	case "active":
		return 0
	case "pending":
		return 1
	default:
		return 2
	}
}

func loopbackRemote(remote string) bool {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Serve 在 addr 上启动只读诊断服务。只接受回环地址。
func Serve(addr string, h *Handler) (*http.Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("admin: 地址不合法：%w", err)
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("admin: 只允许回环地址，当前 %q", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return srv, nil
}
