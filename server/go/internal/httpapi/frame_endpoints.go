package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/frames"
)

// handleDeviceFrame：GET /api/device/frame
//
// 设备用注册时拿到的密钥取自己在用户名下的帧包。未配对的密钥 403；
// 已配对但还没出过帧 404。ETag 是帧版本号，支持 If-None-Match/304。
func (a *API) handleDeviceFrame(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.frames == nil {
		fail(w, 503, "frame not ready", "")
		return
	}
	// 用令牌自己的 KeyID 判权：pending/已吊销的密钥拿不到别人 MAC 的帧。
	data, ver, err := a.frames.CurrentForKey(id.KeyID, id.Dev)
	switch {
	case errors.Is(err, frames.ErrNotPaired):
		fail(w, 403, "forbidden", "设备尚未配对")
		return
	case errors.Is(err, frames.ErrNoFrame):
		w.Header().Set("Retry-After", "30")
		fail(w, 404, "frame not ready", "")
		return
	case err != nil:
		fail(w, 500, "server error", "")
		return
	}

	etag := `"` + strconv.FormatUint(ver, 10) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Frame-Version", strconv.FormatUint(ver, 10))
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// etagMatches 支持 "*"、弱校验前缀和逗号分隔的列表。
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true
		}
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}

// markFramesDirty 只标脏：渲染由 frames 的后台 worker 防抖执行，
// HTTP 请求不等 600ms 的渲染。
func (a *API) markFramesDirty(userID string) {
	if a.frames == nil {
		return
	}
	a.frames.MarkDirty(userID)
}
