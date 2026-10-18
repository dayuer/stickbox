package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/photo"
)

// deviceUser 校验设备令牌仍处于已配对状态，返回它所属用户。
func (a *API) deviceUser(id auth.Identity) (string, bool) {
	k, ok, err := a.store.KeyByID(id.KeyID)
	if err != nil || !ok || k.Disabled || k.Pending ||
		k.Role != string(auth.RoleDevice) || k.Dev != id.Dev || k.UserID == "" {
		return "", false
	}
	return k.UserID, true
}

func (a *API) notifyPlaylist(userID string) {
	if a.frames != nil {
		a.frames.NotifyPlaylist(userID)
	}
}

func (a *API) frameVersion(userID string) photo.FrameVersion {
	if a.frames == nil {
		return nil
	}
	return func(theme string) (uint64, bool) {
		_, ver, ok := a.frames.Current(userID, theme)
		return ver, ok
	}
}

// handleDevicePlaylist：GET /api/device/playlist
func (a *API) handleDevicePlaylist(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	userID, ok := a.deviceUser(id)
	if !ok {
		fail(w, 403, "forbidden", "设备尚未配对")
		return
	}
	m, err := a.photos.Manifest(userID, a.frameVersion(userID))
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	etag := `"` + strconv.FormatInt(m.PLV, 10) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, 200, m)
}

// handlePlaylistPut：PUT /api/playlist，完整期望状态。
func (a *API) handlePlaylistPut(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	var in struct {
		Entries []photo.Entry `json:"entries"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		fail(w, 400, "bad request", "")
		return
	}
	for _, e := range in.Entries {
		// frames 在 main 里总是注入；单测里可能没有，此时只要求非空。
		if e.Kind == "board" && (e.Theme == "" ||
			(a.frames != nil && !a.frames.HasTheme(e.Theme))) {
			fail(w, 400, "bad request", "未知主题："+e.Theme)
			return
		}
	}
	if _, err := a.photos.SetPlaylist(id.UserID, in.Entries); err != nil {
		switch {
		case errors.Is(err, photo.ErrPhotoNotFound):
			fail(w, 404, "not found", "清单里有不属于本用户的照片")
		case errors.Is(err, photo.ErrPlaylistInvalid):
			fail(w, 400, "bad request", err.Error())
		default:
			fail(w, 500, "server error", "")
		}
		return
	}
	a.notifyPlaylist(id.UserID)
	m, err := a.photos.Manifest(id.UserID, a.frameVersion(id.UserID))
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, m)
}

// handlePhotoDelete：DELETE /api/photo/{sha256}
func (a *API) handlePhotoDelete(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	sha := r.PathValue("sha256")
	if !photo.ValidSHA(sha) {
		fail(w, 400, "bad request", "sha256 必须是 64 位小写十六进制")
		return
	}
	plv, found, err := a.photos.Delete(id.UserID, sha)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !found {
		fail(w, 404, "not found", "")
		return
	}
	a.notifyPlaylist(id.UserID)
	writeJSON(w, 200, map[string]any{"ok": 1, "plv": plv})
}

// handleAgentPhotos：GET /api/agent/photos
func (a *API) handleAgentPhotos(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	list, err := a.photos.List(id.UserID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]any{"photos": list})
}

// handlePhotoInit：POST /api/photo/init {"force":false}
func (a *API) handlePhotoInit(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	var in struct {
		Force bool `json:"force"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			fail(w, 400, "bad request", "")
			return
		}
	}
	token, expires, plv, err := a.photos.CreateInitToken(id.UserID, in.Force)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	a.notifyPlaylist(id.UserID)
	writeJSON(w, 200, map[string]any{
		"init_token": token, "force": in.Force,
		"expires_at": expires, "plv": plv,
	})
}

// handleDevicePhoto：GET /api/device/photo/{sha256}，单段 Range。
func (a *API) handleDevicePhoto(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if a.photos == nil {
		fail(w, 503, "photo not ready", "")
		return
	}
	sha := r.PathValue("sha256")
	if !photo.ValidSHA(sha) {
		fail(w, 400, "bad request", "sha256 必须是 64 位小写十六进制")
		return
	}
	userID, ok := a.deviceUser(id)
	if !ok {
		fail(w, 403, "forbidden", "设备尚未配对")
		return
	}
	meta, ok, err := a.photos.PhotoMeta(userID, sha)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		fail(w, 404, "not found", "")
		return
	}
	f, err := os.Open(a.photos.FilePath(userID, sha))
	if err != nil {
		fail(w, 404, "not found", "")
		return
	}
	defer f.Close()

	etag := `"` + sha + `"`
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	size := meta.Size
	start, end, ranged, badRange := parseSingleRange(r.Header.Get("Range"), size)
	if badRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	n := size
	status := http.StatusOK
	if ranged {
		n = end - start + 1
		status = http.StatusPartialContent
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return
		}
	}
	_, _ = io.CopyN(w, f, n)
}

// parseSingleRange 只接受 bytes=n- 和 bytes=a-b；其余（多段、后缀、语法错）
// 都返回 bad=true，由调用方回 416。
func parseSingleRange(h string, size int64) (start, end int64, ranged, bad bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, 0, false, false
	}
	if !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return 0, 0, false, true
	}
	spec := strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 || parts[0] == "" || size <= 0 {
		return 0, 0, false, true
	}
	s, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false, true
	}
	e := size - 1
	if parts[1] != "" {
		e, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || e < s {
			return 0, 0, false, true
		}
		if e >= size {
			e = size - 1
		}
	}
	return s, e, true, false
}
