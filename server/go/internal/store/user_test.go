package store

import "testing"

func TestCreateUser(t *testing.T) {
	s := open(t)
	u, err := s.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.UserByID(u.ID)
	if err != nil || !ok || got.ID != u.ID || got.Created == 0 {
		t.Fatalf("取回 %+v ok=%v err=%v", got, ok, err)
	}
	users, err := s.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("用户列表 = %+v err=%v", users, err)
	}
}

func TestUserByIDNotFound(t *testing.T) {
	s := open(t)
	if _, ok, err := s.UserByID("nope"); ok || err != nil {
		t.Errorf("ok=%v err=%v，期望查不到", ok, err)
	}
}

func TestDevicesOfUserDedupes(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "k1", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})
	s.InsertKey(Key{KeyID: "k2", Dev: "d2", Role: "device", UserID: "u1", Hash: "h"})
	s.InsertKey(Key{KeyID: "k3", Dev: "d3", Role: "device", UserID: "u2", Hash: "h"})
	s.InsertKey(Key{KeyID: "k4", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})

	devs, err := s.DevicesOfUser("u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("得到 %v，期望去重后的 [d1 d2]", devs)
	}
}

// 待配对密钥不属于任何用户，也不能算作设备已绑定。
func TestPendingKeyIsNotOwnership(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "k1", Dev: "d1", Role: "device", UserID: "", Hash: "h", Pending: true})

	if _, ok, _ := s.OwnerOfDevice("d1"); ok {
		t.Error("待配对密钥不该产生 owner")
	}
	if devs, _ := s.DevicesOfUser("u1"); len(devs) != 0 {
		t.Errorf("待配对密钥被列进用户设备：%v", devs)
	}
	if _, ok, _ := s.DeviceMeta("d1"); ok {
		t.Error("待配对密钥不该算有效设备信息")
	}
}

func TestOwnerOfDevice(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "k1", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})
	if owner, ok, _ := s.OwnerOfDevice("d1"); !ok || owner != "u1" {
		t.Errorf("owner=%q ok=%v，期望 u1", owner, ok)
	}
	if _, ok, _ := s.OwnerOfDevice("unbound"); ok {
		t.Error("未绑定的设备不该有 owner")
	}
}

func TestDisabledKeyReleasesDevice(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "k1", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})
	if _, err := s.DisableKeysOfDevice("d1", "device"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OwnerOfDevice("d1"); ok {
		t.Error("吊销后设备仍有归属")
	}
	if devs, _ := s.DevicesOfUser("u1"); len(devs) != 0 {
		t.Errorf("吊销后仍列在名下：%v", devs)
	}
}

// 配对转正：只保留刚转正的那把，旧密钥作废，待配对标记清掉。
func TestActivateAndRevokeExcept(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "old", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})
	s.InsertKey(Key{KeyID: "new", Dev: "d1", Role: "device", UserID: "", Hash: "h", Pending: true})

	if ok, err := s.ActivateDeviceKey("new", "u1"); err != nil || !ok {
		t.Fatalf("转正 ok=%v err=%v", ok, err)
	}
	ids, err := s.DisableDeviceKeysExcept("d1", "new")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "old" {
		t.Errorf("吊销列表 = %v，期望 [old]", ids)
	}
	k, _, _ := s.KeyByID("new")
	if k.Pending || k.UserID != "u1" || k.Disabled {
		t.Errorf("新密钥状态 = %+v", k)
	}
	if owner, ok, _ := s.OwnerOfDevice("d1"); !ok || owner != "u1" {
		t.Errorf("owner=%q ok=%v", owner, ok)
	}
}

// 反复注册时清掉同一台设备之前的待配对密钥。
func TestDisablePendingExcept(t *testing.T) {
	s := open(t)
	s.InsertKey(Key{KeyID: "p1", Dev: "d1", Role: "device", Hash: "h", Pending: true})
	s.InsertKey(Key{KeyID: "p2", Dev: "d1", Role: "device", Hash: "h", Pending: true})
	s.InsertKey(Key{KeyID: "active", Dev: "d1", Role: "device", UserID: "u1", Hash: "h"})

	ids, err := s.DisablePendingDeviceKeysExcept("d1", "p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "p1" {
		t.Errorf("吊销列表 = %v，期望 [p1]", ids)
	}
	k, _, _ := s.KeyByID("active")
	if k.Disabled {
		t.Error("已配对的密钥被误伤")
	}
}
