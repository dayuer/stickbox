package photo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

// 清单里照片条目上限 = 硬配额 + 1 个 board。
const maxEntries = MaxPhotos + 1

// InitTokenTTL 是一次性 SD 初始化授权的有效期。
const InitTokenTTL = 10 * time.Minute

var (
	ErrPlaylistInvalid = errors.New("photo: 清单非法")
	ErrPhotoNotFound   = errors.New("photo: 照片不存在")
)

// Entry 是 PUT /api/playlist 的一项。
type Entry struct {
	Kind      string `json:"kind"`
	Theme     string `json:"theme,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	DurationS int    `json:"duration_s"`
}

// ManifestEntry 是下发给设备的完整条目。
type ManifestEntry struct {
	Kind      string `json:"kind"`
	Theme     string `json:"theme,omitempty"`
	Ver       uint64 `json:"ver,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
	DurationS int    `json:"duration_s"`
	Order     int    `json:"order"`
	URL       string `json:"url,omitempty"`
}

// Manifest 是 GET /api/device/playlist 的响应。
type Manifest struct {
	PLV           int64           `json:"plv"`
	InitToken     string          `json:"init_token,omitempty"`
	InitForce     *bool           `json:"init_force,omitempty"`
	InitExpiresAt int64           `json:"init_expires_at,omitempty"`
	Entries       []ManifestEntry `json:"entries"`
}

// FrameVersion 返回某主题当前帧包版本；没有帧时 ok=false。
type FrameVersion func(theme string) (ver uint64, ok bool)

// Manifest 生成完整清单。board 的 ver 由调用方从 frames 取；照片只返回
// 仍然存在的条目（删除是事务的，这里只是防御）。
func (s *Service) Manifest(userID string, frameVer FrameVersion) (Manifest, error) {
	plv, entries, err := s.st.Playlist(userID)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{PLV: plv, Entries: []ManifestEntry{}}

	// 清单第一次出现或历史数据缺 board 时补一条。
	if len(entries) == 0 || entries[0].Kind != "board" {
		entries = append([]store.PlaylistEntry{{
			Kind: "board", Theme: s.boardTheme, DurationS: 20,
		}}, entries...)
	}

	order := 0
	for _, e := range entries {
		if order >= maxEntries {
			break
		}
		switch e.Kind {
		case "board":
			theme := e.Theme
			if theme == "" {
				theme = s.boardTheme
			}
			me := ManifestEntry{Kind: "board", Theme: theme,
				DurationS: e.DurationS, Order: order}
			if frameVer != nil {
				if v, ok := frameVer(theme); ok {
					me.Ver = v
				}
			}
			m.Entries = append(m.Entries, me)
			order++
		case "photo":
			p, ok, err := s.st.PhotoBySHA(userID, e.SHA256)
			if err != nil {
				return Manifest{}, err
			}
			if !ok {
				continue // 防御：清单引用已不存在的照片
			}
			m.Entries = append(m.Entries, ManifestEntry{
				Kind: "photo", SHA256: p.SHA256, Size: p.Size,
				DurationS: e.DurationS, Order: order,
				URL: "/api/device/photo/" + p.SHA256,
			})
			order++
		}
	}

	// 一次性初始化 token：过期或已被设备用掉就不下发。
	if in, ok, err := s.st.PhotoInit(userID); err != nil {
		return Manifest{}, err
	} else if ok {
		now := s.now().Unix()
		used := false
		if len(in.Token) >= 8 {
			if used, err = s.st.InitDoneForUser(userID, in.Token[:8]); err != nil {
				return Manifest{}, err
			}
		}
		if in.Expires <= now || used {
			if err := s.st.ClearPhotoInit(userID); err != nil {
				return Manifest{}, err
			}
		} else {
			force := in.Force
			m.InitToken = in.Token
			m.InitForce = &force
			m.InitExpiresAt = in.Expires
		}
	}
	return m, nil
}

// SetPlaylist 用完整期望状态替换清单。调用方已校验主题存在；这里校验
// 照片归属、时长、重复和 board 位置。
func (s *Service) SetPlaylist(userID string, in []Entry) (int64, error) {
	if len(in) == 0 || len(in) > maxEntries {
		return 0, fmt.Errorf("%w: 条目数 %d 超出 1–%d", ErrPlaylistInvalid, len(in), maxEntries)
	}
	if in[0].Kind != "board" {
		return 0, fmt.Errorf("%w: 第一条必须是 board", ErrPlaylistInvalid)
	}
	seenSHA := map[string]bool{}
	board := 0
	out := make([]store.PlaylistEntry, 0, len(in))
	for i, e := range in {
		if e.DurationS < 5 || e.DurationS > 600 {
			return 0, fmt.Errorf("%w: 第 %d 项 duration_s=%d 超出 5–600", ErrPlaylistInvalid, i, e.DurationS)
		}
		switch e.Kind {
		case "board":
			board++
			if i != 0 || e.Theme == "" {
				return 0, fmt.Errorf("%w: 第 %d 项 board 位置/主题非法", ErrPlaylistInvalid, i)
			}
			out = append(out, store.PlaylistEntry{
				Kind: "board", Theme: e.Theme, DurationS: e.DurationS,
			})
		case "photo":
			if !validSHA(e.SHA256) || seenSHA[e.SHA256] {
				return 0, fmt.Errorf("%w: 第 %d 项 sha256 非法或重复", ErrPlaylistInvalid, i)
			}
			seenSHA[e.SHA256] = true
			if _, ok, err := s.st.PhotoBySHA(userID, e.SHA256); err != nil {
				return 0, err
			} else if !ok {
				return 0, ErrPhotoNotFound
			}
			out = append(out, store.PlaylistEntry{
				Kind: "photo", SHA256: e.SHA256, DurationS: e.DurationS,
			})
		default:
			return 0, fmt.Errorf("%w: 第 %d 项 kind=%q", ErrPlaylistInvalid, i, e.Kind)
		}
	}
	if board != 1 {
		return 0, fmt.Errorf("%w: board 条目数 %d", ErrPlaylistInvalid, board)
	}
	return s.st.ReplacePlaylist(userID, out)
}

// Delete 删除照片、清单引用与文件。
func (s *Service) Delete(userID, sha string) (plv int64, found bool, err error) {
	plv, found, err = s.st.DeletePhotoWithPlaylist(userID, sha)
	if err != nil || !found {
		return plv, found, err
	}
	// 元数据已删；文件删失败不影响正确性（下次同 sha 上传会覆盖）。
	_ = os.Remove(s.FilePath(userID, sha))
	return plv, true, nil
}

// ListedPhoto 是 GET /api/agent/photos 的一项。
type ListedPhoto struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	UploadedAt int64  `json:"uploaded_at"`
	InPlaylist bool   `json:"in_playlist"`
	DurationS  int    `json:"duration_s,omitempty"`
}

func (s *Service) List(userID string) ([]ListedPhoto, error) {
	photos, err := s.st.PhotosOfUser(userID)
	if err != nil {
		return nil, err
	}
	_, entries, err := s.st.Playlist(userID)
	if err != nil {
		return nil, err
	}
	dur := map[string]int{}
	for _, e := range entries {
		if e.Kind == "photo" {
			dur[e.SHA256] = e.DurationS
		}
	}
	out := make([]ListedPhoto, 0, len(photos))
	for _, p := range photos {
		d, in := dur[p.SHA256]
		out = append(out, ListedPhoto{
			SHA256: p.SHA256, Size: p.Size, Width: p.Width, Height: p.Height,
			UploadedAt: p.Created, InPlaylist: in, DurationS: d,
		})
	}
	return out, nil
}

// CreateInitToken 生成一次性 SD 初始化授权，并推进 plv 让设备去拉清单。
func (s *Service) CreateInitToken(userID string, force bool) (
	token string, expiresAt int64, plv int64, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", 0, 0, err
	}
	token = hex.EncodeToString(b)
	expiresAt = s.now().Add(InitTokenTTL).Unix()
	if err := s.st.SetPhotoInit(userID, store.PhotoInit{
		Token: token, Force: force, Expires: expiresAt,
	}); err != nil {
		return "", 0, 0, err
	}
	plv, err = s.st.BumpPlaylistVersion(userID)
	return token, expiresAt, plv, err
}

// PhotoMeta 返回照片元数据（下载接口用）。
func (s *Service) PhotoMeta(userID, sha string) (store.Photo, bool, error) {
	return s.st.PhotoBySHA(userID, sha)
}

// ValidSHA 判断 64 位小写十六进制内容寻址键。
func ValidSHA(s string) bool { return validSHA(s) }

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
