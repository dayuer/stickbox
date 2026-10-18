package store

import (
	"errors"
	"strings"
	"testing"
)

func TestAddPhotoQuotaAndDedupe(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser()
	p := Photo{SHA256: shaOf("a"), Size: 1000, Width: 1920, Height: 1080, Created: 1}
	created, count, bytes, err := s.AddPhoto(u.ID, p, 2, 3000)
	if err != nil || !created || count != 1 || bytes != 1000 {
		t.Fatalf("第一张 = %v,%d,%d,%v", created, count, bytes, err)
	}
	// 同 sha 重复上传：不占配额，created=false。
	created, count, _, err = s.AddPhoto(u.ID, p, 2, 3000)
	if err != nil || created || count != 1 {
		t.Fatalf("重复 = %v,%d,%v", created, count, err)
	}
	// 张数配额。
	p2 := p
	p2.SHA256 = shaOf("b")
	p2.Size = 1000
	if _, _, _, err := s.AddPhoto(u.ID, p2, 2, 3000); err != nil {
		t.Fatalf("第二张 = %v", err)
	}
	p3 := p
	p3.SHA256 = shaOf("c")
	p3.Size = 1000
	if _, _, _, err := s.AddPhoto(u.ID, p3, 2, 3000); !errors.Is(err, ErrPhotoQuota) {
		t.Fatalf("第三张 err = %v，期望配额错误", err)
	}
	// 字节配额。
	p4 := p
	p4.SHA256 = shaOf("d")
	p4.Size = 2000
	if _, _, _, err := s.AddPhoto(u.ID, p4, 10, 2500); !errors.Is(err, ErrPhotoQuota) {
		t.Fatalf("字节配额 err = %v", err)
	}

	got, ok, err := s.PhotoBySHA(u.ID, p.SHA256)
	if err != nil || !ok || got.Size != 1000 {
		t.Fatalf("PhotoBySHA = %+v ok=%v err=%v", got, ok, err)
	}
	photos, err := s.PhotosOfUser(u.ID)
	if err != nil || len(photos) != 2 {
		t.Fatalf("PhotosOfUser = %d err=%v", len(photos), err)
	}
}

func TestAppendPhotoToPlaylist(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser()
	sha := shaOf("a")
	plv, in, err := s.AppendPhotoToPlaylist(u.ID, sha, "quota", 8)
	if err != nil || !in || plv != 1 {
		t.Fatalf("首次追加 = plv%d in%v err%v", plv, in, err)
	}
	// 重复追加不改变清单和版本。
	plv2, in2, err := s.AppendPhotoToPlaylist(u.ID, sha, "quota", 8)
	if err != nil || !in2 || plv2 != plv {
		t.Fatalf("重复追加 = plv%d in%v err%v", plv2, in2, err)
	}
	sha2 := shaOf("b")
	plv3, _, err := s.AppendPhotoToPlaylist(u.ID, sha2, "quota", 5)
	if err != nil || plv3 != plv+1 {
		t.Fatalf("第二张 = plv%d err%v", plv3, err)
	}
	entries, err := s.PlaylistEntries(u.ID)
	if err != nil || len(entries) != 3 {
		t.Fatalf("清单 = %+v err=%v", entries, err)
	}
	if entries[0].Kind != "board" || entries[0].Pos != 0 || entries[0].Theme != "quota" {
		t.Fatalf("board = %+v", entries[0])
	}
	if entries[1].Kind != "photo" || entries[1].SHA256 != sha || entries[1].DurationS != 8 {
		t.Fatalf("photo1 = %+v", entries[1])
	}
	if entries[2].Pos != 2 || entries[2].SHA256 != sha2 {
		t.Fatalf("photo2 = %+v", entries[2])
	}
}

func shaOf(prefix string) string { return prefix + strings.Repeat("0", 63) }
