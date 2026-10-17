package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

func TestPairNewUserCreatesAgentAndDevice(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")

	agentTok, dev, userID := mustPair(t, h, code, "")
	if dev != "f412fa87c9e0" || userID == "" || agentTok == "" {
		t.Fatalf("配对结果 dev=%q user=%q token=%q", dev, userID, agentTok)
	}
	id, err := dep.v.Verify(agentTok)
	if err != nil || id.Role != auth.RoleAgent || id.UserID != userID {
		t.Errorf("agent 身份 = %+v err=%v", id, err)
	}
	devs := agentDevices(t, h, agentTok)
	if len(devs) != 1 || devs[0].Dev != dev {
		t.Errorf("设备列表 = %+v", devs)
	}
	var note struct {
		Paired bool   `json:"paired"`
		KeyID  string `json:"key_id"`
	}
	json.Unmarshal(dep.pub.pair[dev], &note)
	if !note.Paired || note.KeyID == "" {
		t.Errorf("配对通知 = %s", dep.pub.pair[dev])
	}
}

// 已有 agent 带着令牌配对：新屏并入同一用户，不再签发新令牌。
func TestPairExistingAgentAddsDevice(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code1 := register(t, h, "f412fa87c9e0")
	agentTok, _, user1 := mustPair(t, h, code1, "")

	_, code2 := register(t, h, "aabbccddeeff")
	tok2, dev2, user2 := mustPair(t, h, code2, agentTok)
	if tok2 != "" {
		t.Errorf("已有 agent 配对不该再发令牌，拿到 %q", tok2)
	}
	if dev2 != "aabbccddeeff" || user2 != user1 {
		t.Errorf("dev=%q user=%q，期望同一用户 %q", dev2, user2, user1)
	}
	if devs := agentDevices(t, h, agentTok); len(devs) != 2 {
		t.Errorf("设备列表 = %+v，期望 2 台", devs)
	}
}

// 已绑定的 MAC 重注册：别人输码 409，原主人的 agent 转正并把旧密钥作废。
func TestBoundMacOnlyOwnerCanReRegister(t *testing.T) {
	h, dep := newTestAPI(t)
	oldDevTok, code1 := register(t, h, "f412fa87c9e0")
	agentA, _, userA := mustPair(t, h, code1, "")

	// 另一个 agent 用户 B。
	_, codeB := register(t, h, "aabbccddeeff")
	agentB, _, _ := mustPair(t, h, codeB, "")

	// 设备被擦 NVS 后重新注册。
	newDevTok, code2 := register(t, h, "f412fa87c9e0")
	if rr := tryPair(h, code2, agentB); rr.Code != 409 {
		t.Fatalf("别人配对已绑定设备 = %d，期望 409：%s", rr.Code, rr.Body)
	}
	// 原主人的码仍然有效。
	tok, dev, user := mustPair(t, h, code2, agentA)
	if tok != "" || dev != "f412fa87c9e0" || user != userA {
		t.Errorf("转正结果 dev=%q user=%q token=%q", dev, user, tok)
	}
	if _, err := dep.v.Verify(oldDevTok); err == nil {
		t.Error("旧设备密钥仍然有效")
	}
	if _, err := dep.v.Verify(newDevTok); err != nil {
		t.Errorf("新设备密钥不可用：%v", err)
	}
}

func TestPairCodeIsSingleUse(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	mustPair(t, h, code, "")

	if rr := tryPair(h, code, ""); rr.Code == 200 {
		t.Error("配对码被重复使用了")
	}
}

func TestPairCodeExpires(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")

	pc, ok, _ := dep.store.PairCodeForDev("f412fa87c9e0")
	if !ok {
		t.Fatal("没有配对码记录")
	}
	pc.Expires = time.Now().Add(-time.Minute).Unix()
	if err := dep.store.PutPairCode(pc); err != nil {
		t.Fatal(err)
	}
	if rr := tryPair(h, code, ""); rr.Code != 404 {
		t.Errorf("过期码 = %d，期望 404", rr.Code)
	}
}

// 同一 IP 10 分钟内最多 10 次失败。
func TestPairFailureRateLimit(t *testing.T) {
	h, _ := newTestAPI(t)
	for i := 0; i < 10; i++ {
		if rr := tryPair(h, "000000", ""); rr.Code != 404 {
			t.Fatalf("第 %d 次错误码 = %d，期望 404", i+1, rr.Code)
		}
	}
	if rr := tryPair(h, "000000", ""); rr.Code != 429 {
		t.Errorf("第 11 次失败 = %d，期望 429", rr.Code)
	}
}

func TestPairRejectsBadAgentToken(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	if rr := tryPair(h, code, "aaaa.bbbb"); rr.Code != 401 {
		t.Errorf("坏 agent 令牌 = %d，期望 401", rr.Code)
	}
}

func TestUnpairReleasesDevice(t *testing.T) {
	h, dep := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, dev, _ := mustPair(t, h, code, "")

	rr := post(h, "/api/agent/unpair", `{"dev":"`+dev+`"}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("解绑 = %d %s", rr.Code, rr.Body)
	}
	if devs := agentDevices(t, h, agentTok); len(devs) != 0 {
		t.Errorf("解绑后设备列表 = %+v", devs)
	}
	if _, err := dep.v.Verify(devTok); err == nil {
		t.Error("解绑后设备密钥仍可用")
	}
	keyID, _, _ := auth.SplitToken(devTok)
	var note struct {
		Paired bool     `json:"paired"`
		KeyIDs []string `json:"key_ids"`
	}
	json.Unmarshal(dep.pub.pair[dev], &note)
	if note.Paired || len(note.KeyIDs) != 1 || note.KeyIDs[0] != keyID {
		t.Errorf("解绑通知 = %s，期望包含 key_id=%s", dep.pub.pair[dev], keyID)
	}
	// 解绑后可以重新注册出码。
	_, newCode := register(t, h, "f412fa87c9e0")
	if len(newCode) != 6 {
		t.Error("解绑后重新注册失败")
	}
}

func TestUnpairRejectsForeignDevice(t *testing.T) {
	h, _ := newTestAPI(t)
	_, codeA := register(t, h, "f412fa87c9e0")
	_, codeB := register(t, h, "aabbccddeeff")
	_, devA, _ := mustPair(t, h, codeA, "")
	agentB, _, _ := mustPair(t, h, codeB, "")

	if rr := post(h, "/api/agent/unpair", `{"dev":"`+devA+`"}`, bearer(agentB)); rr.Code != 403 {
		t.Errorf("解绑别人的屏 = %d，期望 403", rr.Code)
	}
}

// 配对成功时把最新用量补发给刚转正的设备。
func TestPairResendsLatestUsage(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code1 := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code1, "")

	// agent 先上报一份用量。
	rr := post(h, "/api/usage", `{"five_hour":{"used_percentage":10,"resets_at":`+
		itoa64(time.Now().Add(time.Hour).Unix())+`}}`, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}

	_, code2 := register(t, h, "aabbccddeeff")
	_, dev2, _ := mustPair(t, h, code2, agentTok)
	if got := dep.pub.usage[dev2]; len(got) == 0 {
		t.Error("配对后没有补发最新用量")
	}
}

// —— 小工具 ——

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
