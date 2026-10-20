package main

// ui 的 JSON API。所有路由都要求 X-Stickbox-Token 头；会改状态的是 POST（Origin 已在 guard 里校验）。
// 响应里永远不包含 agent 令牌（也不返回 config 原文）。

import (
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const pairExpiredHint = "配对码错误或已过期。电视上的码每 10 分钟换新，请输入最新的码。"

type uiError struct {
	OK    bool   `json:"ok"`
	Kind  string `json:"e"`
	Msg   string `json:"msg"`
	Retry int    `json:"retry_after,omitempty"` // 秒
}

func (s *uiServer) fail(w http.ResponseWriter, status int, kind, msg string) {
	s.writeJSON(w, status, uiError{Kind: kind, Msg: msg})
}

// api 包一层：方法、token、（POST）Content-Type。
func (s *uiServer) api(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r.Header.Get("X-Stickbox-Token")) {
			s.fail(w, http.StatusUnauthorized, "unauthorized", "缺少或错误的 token：请用 stickbox-agent ui 打印的地址打开页面")
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			s.fail(w, http.StatusMethodNotAllowed, "method", "方法不允许")
			return
		}
		if method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			s.fail(w, http.StatusUnsupportedMediaType, "content_type", "请求需为 application/json")
			return
		}
		h(w, r)
	}
}

func (s *uiServer) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/status", s.api(http.MethodGet, s.handleStatus))
	mux.HandleFunc("/api/devices", s.api(http.MethodGet, s.handleDevices))
	mux.HandleFunc("/api/discover", s.api(http.MethodGet, s.handleDiscover))
	mux.HandleFunc("/api/pair-device", s.api(http.MethodPost, s.handlePairDevice))
	mux.HandleFunc("/api/pair", s.api(http.MethodPost, s.handlePair))
	mux.HandleFunc("/api/unpair", s.api(http.MethodPost, s.handleUnpair))
	mux.HandleFunc("/api/run-once", s.api(http.MethodPost, s.handleRunOnce))
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// ---- 状态 ----

func (s *uiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	c, err := loadConfig()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	server, nerr := normalizeServer(c.Server)
	if nerr != nil {
		server = c.Server
	}
	if s.opts.server != "" && c.Token == "" {
		if n, err := normalizeServer(s.opts.server); err == nil {
			server = n
		}
	}
	st := loadState()
	out := map[string]any{
		"version":              version,
		"server":               server,
		"paired":               c.Token != "",
		"paired_at":            c.PairedAt,
		"name":                 c.Name,
		"insecure":             c.Insecure || s.opts.insecure,
		"last_success_at":      st.LastSuccessAt,
		"last_error":           st.LastError,
		"last_error_at":        st.LastErrorAt,
		"claude_auth":          st.ClaudeAuth,
		"claude_auth_at":       st.ClaudeAuthAt,
		"consecutive_failures": st.ConsecutiveFailures,
		"next_retry_at":        st.NextRetryAt,
		"cooldown":             st.Cooldown,
		"lines":                st.statusLines(),
	}
	claude := c.ClaudePath
	if claude == "" {
		claude, _ = exec.LookPath("claude")
	}
	out["claude_found"] = claude != ""
	installed, _ := serviceInstalled()
	out["service_installed"] = installed
	s.writeJSON(w, http.StatusOK, out)
}

// ---- 设备 ----

// classifyErr 把 pair/unpair/devices 的错误翻成 HTTP 状态 + 类别 + 中文提示。
func classifyErr(err error) (int, string, string) {
	var te *transportError
	if errors.As(err, &te) {
		return http.StatusBadGateway, "network", "连不上服务器，请检查网络后重试。（" + te.Error() + "）"
	}
	if errors.Is(err, errNotPaired) {
		return http.StatusConflict, "not_paired", "尚未配对：请先输入电视上的配对码"
	}
	var pp postPairError
	if errors.As(err, &pp) {
		return http.StatusInternalServerError, "local", err.Error()
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusUnauthorized:
			return http.StatusConflict, "stale_token", "本机保存的令牌已失效：请在终端运行 stickbox-agent logout，再重新配对"
		case ae.Code == "bad code":
			return http.StatusBadRequest, "bad_code", pairExpiredHint
		case ae.Code == "rate limited":
			return http.StatusTooManyRequests, "rate_limited", "服务端提示尝试次数过多，请稍等一会儿再试"
		case ae.Code == "device bound":
			return http.StatusConflict, "device_bound", "这块屏已绑定到其他用户"
		}
		return http.StatusBadGateway, "server", ae.Error()
	}
	return http.StatusInternalServerError, "other", err.Error()
}

func (s *uiServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	c, err := loadConfig()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	devices, err := listDevices(c)
	if err != nil {
		st, kind, msg := classifyErr(err)
		s.fail(w, st, kind, msg)
		return
	}
	if devices == nil {
		devices = []deviceRow{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// ---- 配对限速 ----

// pairBlocked：窗口内失败次数是否已到上限；返回还需等待的秒数。
func (s *uiServer) pairBlocked() (bool, int) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	now := s.now()
	kept := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < uiPairFailWindow {
			kept = append(kept, t)
		}
	}
	s.failures = kept
	if len(kept) < uiPairFailLimit {
		return false, 0
	}
	wait := uiPairFailWindow - now.Sub(kept[0])
	secs := int(wait/time.Second) + 1
	return true, secs
}

func (s *uiServer) recordPairFail() {
	s.failMu.Lock()
	s.failures = append(s.failures, s.now())
	s.failMu.Unlock()
}

// ---- 配对 ----

func (s *uiServer) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, "bad_request", "请求格式不对")
		return
	}
	code := strings.TrimSpace(req.Code)
	if !validPairCode(code) {
		s.fail(w, http.StatusBadRequest, "bad_format", "配对码应为 6 位数字")
		return
	}
	if blocked, wait := s.pairBlocked(); blocked {
		s.writeJSON(w, http.StatusTooManyRequests, uiError{
			Kind: "rate_limited", Retry: wait,
			Msg: "尝试次数过多，请稍等片刻再试",
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := loadPairConfig(s.opts.server, s.opts.insecure)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "config", err.Error())
		return
	}
	name := req.Name
	if strings.TrimSpace(name) == "" {
		name = s.opts.name
	}
	dev, newUser, err := pairAndSave(c, code, resolveAgentName(name))
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			s.recordPairFail() // 只有服务端明确拒绝才算一次猜码
		}
		st, kind, msg := classifyErr(err)
		s.logf("网页配对失败：%s", kind)
		s.fail(w, st, kind, msg)
		return
	}
	s.addSecret(c.Token)
	s.markPaired()
	s.logf("网页配对成功：%s", dev)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dev": dev, "new_user": newUser,
		"msg": "已配对，电视会在几秒内切到看板",
	})
}

// ---- 局域网发现 ----

type foundView struct {
	Dev   string `json:"dev"`
	Short string `json:"short"`
	IP    string `json:"ip"`
	FW    string `json:"fw"`
}

func (s *uiServer) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.discMu.TryLock() {
		s.fail(w, http.StatusConflict, "busy", "正在搜索，请稍候")
		return
	}
	defer s.discMu.Unlock()
	defer s.touch()
	devs, err := s.disc.Discover(r.Context())
	if err != nil {
		s.logf("局域网发现失败：%v", err)
		s.fail(w, http.StatusBadGateway, "discover", "搜索失败："+truncateRunes(err.Error(), 200))
		return
	}
	s.foundMu.Lock()
	s.found = map[string]foundDevice{}
	view := make([]foundView, 0, len(devs))
	for _, d := range devs {
		s.found[d.Dev] = d
		view = append(view, foundView{Dev: d.Dev, Short: d.Short(), IP: d.IP, FW: d.FW})
	}
	s.foundMu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"devices": view})
}

// handlePairDevice：对一块已发现的设备读码并配对。设备地址只取服务端记下的发现结果；
// 码只在本进程内存里经过，不写日志、不回传页面。
func (s *uiServer) handlePairDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dev  string `json:"dev"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, "bad_request", "请求格式不对")
		return
	}
	s.foundMu.Lock()
	d, ok := s.found[strings.ToLower(strings.TrimSpace(req.Dev))]
	s.foundMu.Unlock()
	if !ok {
		s.fail(w, http.StatusNotFound, "not_found", "这块屏不在最近的搜索结果里，请重新搜索")
		return
	}
	if blocked, wait := s.pairBlocked(); blocked {
		s.writeJSON(w, http.StatusTooManyRequests, uiError{Kind: "rate_limited", Retry: wait, Msg: "尝试次数过多，请稍等片刻再试"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := loadPairConfig(s.opts.server, s.opts.insecure)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "config", err.Error())
		return
	}
	name := req.Name
	if strings.TrimSpace(name) == "" {
		name = s.opts.name
	}
	dev, newUser, err := pairFound(r.Context(), c, s.codes, d, resolveAgentName(name))
	if err != nil {
		switch {
		case errors.Is(err, errNotPending):
			s.fail(w, http.StatusConflict, "not_pending", errNotPending.Error())
		case errors.Is(err, errCodeExpired):
			s.fail(w, http.StatusConflict, "code_expired", errCodeExpired.Error())
		default:
			var ae *apiError
			var te *transportError
			if errors.As(err, &ae) {
				s.recordPairFail()
			}
			if errors.As(err, &ae) || errors.As(err, &te) || errors.Is(err, errNotPaired) {
				st, kind, msg := classifyErr(err)
				s.fail(w, st, kind, msg)
			} else {
				s.fail(w, http.StatusBadGateway, "device", truncateRunes(err.Error(), 200))
			}
		}
		s.logf("发现配对失败：%s", d.Short())
		return
	}
	s.addSecret(c.Token)
	s.markPaired()
	s.logf("发现配对成功：%s", dev)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dev": dev, "new_user": newUser,
		"msg": "已配对，电视会在几秒内切到看板",
	})
}

// ---- 解绑 ----

func (s *uiServer) handleUnpair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dev string `json:"dev"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, "bad_request", "请求格式不对")
		return
	}
	dev := strings.TrimSpace(req.Dev)
	if dev == "" || len([]rune(dev)) > 64 || strings.ContainsAny(dev, "\r\n\x00") {
		s.fail(w, http.StatusBadRequest, "bad_request", "设备编号不合法")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := loadConfig()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	if s.opts.server != "" && c.Token == "" {
		c.Server = s.opts.server
	}
	if err := unpairDevice(c, dev); err != nil {
		st, kind, msg := classifyErr(err)
		var ae *apiError
		if errors.As(err, &ae) && st == http.StatusBadGateway {
			switch ae.Code {
			case "forbidden":
				msg = "无权解绑这台设备"
			case "not found":
				msg = "名下没有这台设备（可能已经解绑）"
			}
		}
		s.fail(w, st, kind, msg)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已解绑 " + dev + "；屏幕会重新显示配对码"})
}

// ---- 立即上报一轮 ----

func (s *uiServer) handleRunOnce(w http.ResponseWriter, r *http.Request) {
	if !s.runMu.TryLock() {
		s.fail(w, http.StatusConflict, "busy", "上一轮还在进行，请稍候")
		return
	}
	defer s.runMu.Unlock()
	defer s.touch()

	c, err := loadConfig()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "config", err.Error())
		return
	}
	if err := requireToken(c); err != nil {
		st, kind, msg := classifyErr(err)
		s.fail(w, st, kind, msg)
		return
	}
	err = s.runRound(c, s.logf)
	var se *skipError
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已上报一轮"})
	case errors.As(err, &se):
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "msg": "本轮没有可报的数据：" + se.reason})
	default:
		s.fail(w, http.StatusBadGateway, "round", truncateRunes(err.Error(), 300))
	}
}
