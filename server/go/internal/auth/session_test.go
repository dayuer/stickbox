package auth

import (
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIssueAgentKeyThenVerify(t *testing.T) {
	st := newStore(t)
	v := NewVerifier(st)
	token, err := IssueAgentKey(st, "u1", "mac")
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleAgent || id.UserID != "u1" {
		t.Errorf("身份 = %+v", id)
	}
}

// 设备注册签发的是一把待配对密钥：dev 已绑定，但 user_id 为空。
func TestIssueDeviceKeyPendingThenActivate(t *testing.T) {
	st := newStore(t)
	v := NewVerifier(st)
	token, keyID, err := IssueDeviceKey(st, "", "f412fa87c9e0", "", true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleDevice || id.Dev != "f412fa87c9e0" || id.UserID != "" {
		t.Errorf("待配对身份 = %+v", id)
	}

	if ok, err := st.ActivateDeviceKey(keyID, "u1"); err != nil || !ok {
		t.Fatalf("转正 ok=%v err=%v", ok, err)
	}
	v.Invalidate(keyID)
	id, err = v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "u1" {
		t.Errorf("转正后 user = %q，期望 u1", id.UserID)
	}
	k, _, _ := st.KeyByID(keyID)
	if k.Pending {
		t.Error("转正后 pending 仍为 true")
	}
}

// 配对转正时旧主人的密钥必须立刻失效。
func TestRevokeDeviceKeysExcept(t *testing.T) {
	st := newStore(t)
	v := NewVerifier(st)
	old, _, _ := IssueDeviceKey(st, "u1", "dev1", "", false)
	freshToken, freshID, _ := IssueDeviceKey(st, "", "dev1", "", true)
	v.Verify(old)
	v.Verify(freshToken)

	if ok, err := st.ActivateDeviceKey(freshID, "u1"); err != nil || !ok {
		t.Fatalf("转正 ok=%v err=%v", ok, err)
	}
	if err := RevokeDeviceKeysExcept(st, v, "dev1", freshID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(old); err == nil {
		t.Error("旧密钥仍可用")
	}
	if _, err := v.Verify(freshToken); err != nil {
		t.Errorf("新密钥被误伤：%v", err)
	}
}
