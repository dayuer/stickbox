package photostatus

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

type recNotifier struct {
	mu   sync.Mutex
	user []string
}

func (r *recNotifier) NotifyPlaylist(userID string) {
	r.mu.Lock()
	r.user = append(r.user, userID)
	r.mu.Unlock()
}

func (r *recNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.user)
}

func TestPersistAndInitDone(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	dev := "f412fa87c9e0"
	if err := st.InsertKey(store.Key{
		KeyID: "k1", Dev: dev, Role: "device", UserID: u.ID, Hash: "h",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPhotoInit(u.ID, store.PhotoInit{
		Token: "abcdef0123456789", Expires: time.Now().Add(time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	nt := &recNotifier{}
	svc := New(st, nt)
	svc.Start()
	defer svc.Close()

	svc.Update(dev, Status{
		Cap: true, Ready: true, PLV: 3, Have: 5,
		Bad: []string{"a1b2c3d4"}, FreeKB: 1024, State: "ok",
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if dp, ok, _ := st.DevicePhoto(dev); ok && dp.PLV == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("状态没落库")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, ok := svc.Get(dev); !ok || !got.Ready || got.Have != 5 {
		t.Fatalf("Get = %+v ok=%v", got, ok)
	}

	// init_done 命中 token 前 8 位：清 token、推进 plv、通知。
	before, _ := st.PlaylistVersion(u.ID)
	svc.Update(dev, Status{Cap: true, Ready: true, InitDone: "abcdef01", State: "ok"})
	for {
		if _, ok, _ := st.PhotoInit(u.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("token 没有被清除")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, _ := st.PlaylistVersion(u.ID)
	if after != before+1 {
		t.Fatalf("plv = %d，期望 %d", after, before+1)
	}
	if nt.count() == 0 {
		t.Fatal("没有通知设备")
	}
}
