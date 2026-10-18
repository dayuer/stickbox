package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/photo"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// handlePhotoPost：POST /api/photo?show=1&secs=8
//
// 原始图片字节（不是 multipart）。服务端负责方向校正、缩放、剥元数据和
// 重新编码；sha256/size 都是最终 JPEG 的。
func (a *API) handlePhotoPost(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if ct != "image/jpeg" && ct != "image/png" {
		fail(w, 415, "unsupported media type", "只接受 image/jpeg 或 image/png")
		return
	}
	// 先看 Content-Length：没有它就不能安全地限流，也说明调用方不是 agent。
	if r.ContentLength <= 0 {
		fail(w, 400, "bad request", "需要 Content-Length")
		return
	}
	if r.ContentLength > photo.MaxUploadBytes {
		fail(w, 413, "too large", "单张上限 20MiB")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, photo.MaxUploadBytes+1))
	if err != nil {
		fail(w, 413, "too large", "单张上限 20MiB")
		return
	}
	if len(raw) > photo.MaxUploadBytes {
		fail(w, 413, "too large", "单张上限 20MiB")
		return
	}

	show := true
	if v := r.URL.Query().Get("show"); v != "" {
		switch v {
		case "1":
		case "0":
			show = false
		default:
			fail(w, 400, "bad request", "show 只能是 0 或 1")
			return
		}
	}
	secs := 8
	if v := r.URL.Query().Get("secs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(w, 400, "bad request", "secs 必须是整数")
			return
		}
		secs = n
	}

	res, err := a.photos.Upload(id.UserID, raw, ct, show, secs)
	switch {
	case err == nil:
	case errors.Is(err, photo.ErrUnsupported):
		fail(w, 415, "unsupported media type", err.Error())
		return
	case errors.Is(err, photo.ErrDecode), errors.Is(err, photo.ErrBadDuration):
		fail(w, 400, "bad request", err.Error())
		return
	case errors.Is(err, photo.ErrTooManyPixels), errors.Is(err, photo.ErrCannotCompress):
		fail(w, 422, "unprocessable image", err.Error())
		return
	case errors.Is(err, store.ErrPhotoQuota):
		fail(w, 409, "quota exceeded", "每用户最多 200 张、64MiB")
		return
	default:
		fail(w, 500, "server error", "")
		return
	}

	// 清单变了才需要通知；重复上传且已在清单里时也发一次，代价很小。
	if show {
		a.notifyPlaylist(id.UserID)
	}

	code := http.StatusCreated
	if !res.Created {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"sha256": res.SHA256, "size": res.Size,
		"width": res.Width, "height": res.Height,
		"plv": res.PLV, "in_playlist": res.InPlaylist,
	})
}
