package broker

import (
	"errors"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// fakeClaimer 记下每次调用，好断言「拒绝的时候压根没去绑」。
type fakeClaimer struct {
	owner    string
	bound    bool
	ownerErr error
	claimOK  bool
	claimErr error

	claimCalls []string // 每次 ClaimKey 传进来的 dev
}

func (f *fakeClaimer) OwnerOfDevice(dev string) (string, bool, error) {
	return f.owner, f.bound, f.ownerErr
}

func (f *fakeClaimer) ClaimKey(keyID, dev string) (bool, error) {
	f.claimCalls = append(f.claimCalls, dev)
	return f.claimOK, f.claimErr
}

func newClaimHook(c Claimer) *Hook {
	return &Hook{
		v:     auth.NewVerifier(nil),
		mem:   NewMembership(nil),
		claim: c,
		ids:   newIDTable(),
	}
}

// 待认领的密钥（Dev 为空），设备自报 dev，绑定成功。
func TestClaimBindsKeyToReportedDev(t *testing.T) {
	c := &fakeClaimer{claimOK: true}
	h := newClaimHook(c)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

	got, ok := h.claimKey(id, "f412fa87c9e0")
	if !ok {
		t.Fatal("认领应当成功")
	}
	if got.Dev != "f412fa87c9e0" {
		t.Errorf("绑定后的 dev = %q", got.Dev)
	}
	if len(c.claimCalls) != 1 || c.claimCalls[0] != "f412fa87c9e0" {
		t.Errorf("ClaimKey 调用记录 = %v", c.claimCalls)
	}
}

// username 不是合法 dev 就拒绝，而且不去动数据库。
//
// 这一条是安全边界：dev 会被拼进 MQTT 的 topic（printer/{dev}/...），
// 放一个畸形值进去等于让它去匹配别人的 topic。
func TestClaimRejectsMalformedDev(t *testing.T) {
	for _, bad := range []string{"", "XYZ", "F412FA87C9E0", "f412fa87c9e",
		"f412fa87c9e0f", "../../etc", "aaa/bbb"} {
		c := &fakeClaimer{claimOK: true}
		h := newClaimHook(c)
		id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

		if _, ok := h.claimKey(id, bad); ok {
			t.Errorf("username=%q 不该被接受", bad)
		}
		if len(c.claimCalls) != 0 {
			t.Errorf("username=%q 被拒了却还是去绑了库", bad)
		}
	}
}

// 抢绑防护：设备已属于别人就拒绝。
//
// 这条判定从 enroll 挪到了这里——App 不带 dev 签发密钥时服务端还不知道
// 要绑哪台，只能等设备连上来才判。不设防的话，任何人把自己的密钥写进
// 别人的设备就能顶掉绑定。
func TestClaimRejectsDeviceOwnedByAnother(t *testing.T) {
	c := &fakeClaimer{owner: "someone-else", bound: true, claimOK: true}
	h := newClaimHook(c)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

	if _, ok := h.claimKey(id, "f412fa87c9e0"); ok {
		t.Error("设备属于别人时不该认领成功")
	}
	if len(c.claimCalls) != 0 {
		t.Error("拒绝了却还是去绑了库")
	}
}

// 同一个用户的设备重新配网：允许。
func TestClaimAllowsOwnDevice(t *testing.T) {
	c := &fakeClaimer{owner: "u1", bound: true, claimOK: true}
	h := newClaimHook(c)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

	if _, ok := h.claimKey(id, "f412fa87c9e0"); !ok {
		t.Error("自己的设备应当可以认领")
	}
}

// 密钥已被别的设备抢先认领（SQL 里 dev=” 的条件没命中）。
func TestClaimRejectsAlreadyClaimed(t *testing.T) {
	c := &fakeClaimer{claimOK: false}
	h := newClaimHook(c)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

	if _, ok := h.claimKey(id, "f412fa87c9e0"); ok {
		t.Error("已被认领的密钥不该再次认领成功")
	}
}

// 查库出错时拒绝，不能放行。
func TestClaimRejectsOnStoreError(t *testing.T) {
	c := &fakeClaimer{ownerErr: errors.New("boom"), claimOK: true}
	h := newClaimHook(c)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}

	if _, ok := h.claimKey(id, "f412fa87c9e0"); ok {
		t.Error("查不出归属时应当拒绝，不能默认放行")
	}
}

// 没有 Claimer 时拒绝——宁可连不上，也不要放一个没绑定的身份进来。
func TestClaimRejectsWithoutClaimer(t *testing.T) {
	h := newClaimHook(nil)
	id := auth.Identity{Role: auth.RoleDevice, UserID: "u1", KeyID: "k1"}
	if _, ok := h.claimKey(id, "f412fa87c9e0"); ok {
		t.Error("没有 Claimer 时不该放行")
	}
}
