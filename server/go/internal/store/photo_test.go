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

func TestReplaceAndDeletePlaylist(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser()
	sha1, sha2 := shaOf("a"), shaOf("b")
	for _, sha := range []string{sha1, sha2} {
		if _, _, _, err := s.AddPhoto(u.ID, Photo{SHA256: sha, Size: 1, Width: 1920, Height: 1080}, 10, 1000); err != nil {
			t.Fatal(err)
		}
	}
	plv, err := s.ReplacePlaylist(u.ID, []PlaylistEntry{
		{Kind: "board", Theme: "quota", DurationS: 20},
		{Kind: "photo", SHA256: sha2, DurationS: 5},
		{Kind: "photo", SHA256: sha1, DurationS: 9},
	})
	if err != nil || plv != 1 {
		t.Fatalf("Replace = plv%d err%v", plv, err)
	}
	entries, _ := s.PlaylistEntries(u.ID)
	if len(entries) != 3 || entries[1].SHA256 != sha2 || entries[1].Pos != 1 {
		t.Fatalf("entries = %+v", entries)
	}

	// 删除一张：照片与清单引用都消失，剩余 pos 连续，plv 前进。
	plv2, found, err := s.DeletePhotoWithPlaylist(u.ID, sha2)
	if err != nil || !found || plv2 != plv+1 {
		t.Fatalf("删除 = plv%d found%v err%v", plv2, found, err)
	}
	entries, _ = s.PlaylistEntries(u.ID)
	if len(entries) != 2 || entries[0].Kind != "board" || entries[1].SHA256 != sha1 || entries[1].Pos != 1 {
		t.Fatalf("删除后清单 = %+v", entries)
	}
	if _, ok, _ := s.PhotoBySHA(u.ID, sha2); ok {
		t.Fatal("照片元数据还在")
	}
	// 再删一次：found=false 且 plv 不变。
	plv3, found, err := s.DeletePhotoWithPlaylist(u.ID, sha2)
	if err != nil || found || plv3 != 0 {
		t.Fatalf("重复删除 = plv%d found%v err%v", plv3, found, err)
	}
}

func TestPhotoInitAndDeviceStatus(t *testing.T) {
	s := open(t)
	u, _ := s.CreateUser()
	if _, ok, _ := s.PhotoInit(u.ID); ok {
		t.Fatal("初始不该有 token")
	}
	if err := s.SetPhotoInit(u.ID, PhotoInit{Token: "abcdef0123456789", Force: true, Expires: 123}); err != nil {
		t.Fatal(err)
	}
	in, ok, err := s.PhotoInit(u.ID)
	if err != nil || !ok || in.Token != "abcdef0123456789" || !in.Force || in.Expires != 123 {
		t.Fatalf("PhotoInit = %+v ok%v err%v", in, ok, err)
	}
	if done, _ := s.InitDoneForUser(u.ID, "abcdef01"); done {
		t.Fatal("还没上报就用掉了")
	}
	if err := s.PutDevicePhoto("dev1", DevicePhoto{
		UserID: u.ID, Cap: true, Ready: true, PLV: 7, Have: 3,
		Bad: []string{"a1b2c3d4", "ffffffff"}, FreeKB: 1024,
		State: "ok", InitDone: "abcdef01", Updated: 9,
	}); err != nil {
		t.Fatal(err)
	}
	dp, ok, err := s.DevicePhoto("dev1")
	if err != nil || !ok || !dp.Cap || !dp.Ready || dp.PLV != 7 || len(dp.Bad) != 2 ||
		dp.State != "ok" || dp.InitDone != "abcdef01" {
		t.Fatalf("DevicePhoto = %+v ok%v err%v", dp, ok, err)
	}
	if done, _ := s.InitDoneForUser(u.ID, "abcdef01"); !done {
		t.Fatal("init_done 没匹配到")
	}
	if err := s.ClearPhotoInit(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PhotoInit(u.ID); ok {
		t.Fatal("token 没清掉")
	}
}
