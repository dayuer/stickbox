package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

func TestRegisterIssuesPendingKeyAndCode(t *testing.T) {
	h, dep := newTestAPI(t)
	token, code := register(t, h, "f412fa87c9e0")

	id, err := dep.v.Verify(token)
	if err != nil {
		t.Fatalf("设备密钥不可用：%v", err)
	}
	if id.Role != auth.RoleDevice || id.Dev != "f412fa87c9e0" || id.UserID != "" {
		t.Errorf("身份 = %+v", id)
	}
	k, _, _ := dep.store.KeyByID(id.KeyID)
	if !k.Pending {
		t.Error("注册签发的密钥应当是待配对")
	}
	pc, ok, _ := dep.store.PairCodeForDev("f412fa87c9e0")
	if !ok || pc.KeyID != id.KeyID || pc.Expires == 0 {
		t.Errorf("配对码记录 = %+v ok=%v", pc, ok)
	}
	if len(code) != 6 {
		t.Errorf("码 = %q", code)
	}
}

func TestRegisterRejectsBadDev(t *testing.T) {
	h, _ := newTestAPI(t)
	for _, dev := range []string{"", "XYZ", "F412FA87C9E0", "f412fa87c9e", "f412fa87c9e0f"} {
		rr := post(h, "/api/device/register", `{"dev":"`+dev+`"}`, nil)
		if rr.Code != 400 {
			t.Errorf("dev=%q → %d，期望 400", dev, rr.Code)
		}
	}
}

// 同一台设备 10 分钟内最多注册 5 次。
func TestRegisterRateLimitedPerDev(t *testing.T) {
	h, _ := newTestAPI(t)
	for i := 0; i < 5; i++ {
		if rr := post(h, "/api/device/register", `{"dev":"f412fa87c9e0"}`, nil); rr.Code != 200 {
			t.Fatalf("第 %d 次注册 = %d %s", i+1, rr.Code, rr.Body)
		}
	}
	if rr := post(h, "/api/device/register", `{"dev":"f412fa87c9e0"}`, nil); rr.Code != 429 {
		t.Errorf("第 6 次注册 = %d，期望 429", rr.Code)
	}
}

// 设备反复重注册：旧的待配对密钥作废，旧码被新码替换。
func TestReRegisterRotatesPendingKey(t *testing.T) {
	h, dep := newTestAPI(t)
	first, _ := register(t, h, "f412fa87c9e0")
	firstID, _ := dep.v.Verify(first)

	second, _ := register(t, h, "f412fa87c9e0")
	if _, err := dep.v.Verify(first); err == nil {
		t.Error("旧的待配对密钥仍可用")
	}
	if _, err := dep.v.Verify(second); err != nil {
		t.Errorf("新的待配对密钥不可用：%v", err)
	}
	k, _, _ := dep.store.KeyByID(firstID.KeyID)
	if !k.Disabled {
		t.Error("旧的待配对密钥没被吊销")
	}
	pc, _, _ := dep.store.PairCodeForDev("f412fa87c9e0")
	if pc.CodeHash == "" {
		t.Error("新配对码没写进去")
	}
}

func TestPairCodeNeedsDeviceAuth(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "aabbccddeeff")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := post(h, "/api/device/pair-code", `{}`, nil); rr.Code != 401 {
		t.Errorf("无令牌 = %d，期望 401", rr.Code)
	}
	if rr := post(h, "/api/device/pair-code", `{}`, bearer(agentTok)); rr.Code != 401 {
		t.Errorf("agent 令牌 = %d，期望 401", rr.Code)
	}
}

func TestPairCodeRotatesWhilePending(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, oldCode := register(t, h, "f412fa87c9e0")

	rr := post(h, "/api/device/pair-code", `{}`, bearer(devTok))
	if rr.Code != 200 {
		t.Fatalf("换码 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Code string `json:"code"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Code) != 6 {
		t.Fatalf("新码 = %q", out.Code)
	}
	pc, _, _ := dep.store.PairCodeForDev("f412fa87c9e0")
	if pc.CodeHash == "" {
		t.Error("新码没落库")
	}
	// 旧码不能再配对。
	if rr := tryPair(h, oldCode, ""); rr.Code == 200 {
		t.Error("旧码仍然配对成功")
	}
}

// 已配对设备再用旧密钥换码：409。
func TestPairCodeRejectsPairedDevice(t *testing.T) {
	h, _ := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	mustPair(t, h, code, "")

	if rr := post(h, "/api/device/pair-code", `{}`, bearer(devTok)); rr.Code != 409 {
		t.Errorf("已配对设备换码 = %d，期望 409", rr.Code)
	}
}
