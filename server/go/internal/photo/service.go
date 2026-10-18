package photo

import (
	"os"
	"path/filepath"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

const (
	// MaxPhotos 和 MaxUserBytes 是每用户硬配额（USB-TV 裁决）。
	MaxPhotos    = 200
	MaxUserBytes = 64 << 20
)

// Store 是照片服务对存储的窄依赖。
type Store interface {
	AddPhoto(userID string, p store.Photo, maxCount int, maxBytes int64) (
		created bool, count int, bytes int64, err error)
	PhotoBySHA(userID, sha string) (store.Photo, bool, error)
	AppendPhotoToPlaylist(userID, sha, boardTheme string, durationS int) (
		plv int64, in bool, err error)
}

// Service 处理上传并落盘。文件路径：<root>/photos/<userID>/<sha256>.jpg。
type Service struct {
	root       string
	st         Store
	boardTheme string
	now        func() time.Time
}

func New(root string, st Store, boardTheme string) *Service {
	return &Service{root: root, st: st, boardTheme: boardTheme, now: time.Now}
}

// UploadResult 是 POST /api/photo 的结果。
type UploadResult struct {
	SHA256     string
	Size       int64
	Width      int
	Height     int
	PLV        int64
	InPlaylist bool
	Created    bool
}

// Upload 处理并保存一张照片；show 为真时追加到清单末尾。
func (s *Service) Upload(userID string, data []byte, contentType string,
	show bool, secs int) (UploadResult, error) {
	if secs < 5 || secs > 600 {
		return UploadResult{}, ErrBadDuration
	}
	p, err := Process(data, contentType)
	if err != nil {
		return UploadResult{}, err
	}
	dir := filepath.Join(s.root, "photos", userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return UploadResult{}, err
	}
	final := filepath.Join(dir, p.SHA256+".jpg")

	created, _, _, err := s.st.AddPhoto(userID, store.Photo{
		SHA256: p.SHA256, Size: p.Size, Width: p.Width, Height: p.Height,
		Created: s.now().Unix(),
	}, MaxPhotos, MaxUserBytes)
	if err != nil {
		return UploadResult{}, err
	}
	// 去重命中也确保文件存在（磁盘被清理过时能自愈）。
	if _, statErr := os.Stat(final); statErr != nil {
		tmp, err := os.CreateTemp(dir, ".upload-*.jpg")
		if err != nil {
			return UploadResult{}, err
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
			return UploadResult{}, err
		}
		if _, err := tmp.Write(p.JPEG); err != nil {
			tmp.Close()
			return UploadResult{}, err
		}
		if err := tmp.Close(); err != nil {
			return UploadResult{}, err
		}
		if err := os.Rename(name, final); err != nil {
			return UploadResult{}, err
		}
		ok = true
	}

	res := UploadResult{
		SHA256: p.SHA256, Size: p.Size, Width: p.Width, Height: p.Height,
		Created: created,
	}
	if show {
		plv, in, err := s.st.AppendPhotoToPlaylist(userID, p.SHA256, s.boardTheme, secs)
		if err != nil {
			return UploadResult{}, err
		}
		res.PLV, res.InPlaylist = plv, in
	}
	return res, nil
}

// FilePath 返回照片最终 JPEG 的路径（下载接口用）。
func (s *Service) FilePath(userID, sha string) string {
	return filepath.Join(s.root, "photos", userID, sha+".jpg")
}
