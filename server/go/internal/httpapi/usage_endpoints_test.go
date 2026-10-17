package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func reportJSON(now time.Time, pct float64) string {
	return fmt.Sprintf(`{"five_hour":{"used_percentage":%g,"resets_at":%d},`+
		`"seven_day":{"used_percentage":%g,"resets_at":%d},`+
		`"limits":[{"kind":"weekly_all","percent":%g}]}`,
		pct, now.Add(time.Hour).Unix(), pct/2, now.Add(48*time.Hour).Unix(), pct/2)
}

// agent 上报 → 落库 → agent 读取。
func TestUsagePostAndAgentRead(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := get(h, "/api/agent/usage", bearer(agentTok)); rr.Code != 404 {
		t.Fatalf("还没上报时 = %d，期望 404", rr.Code)
	}

	body := reportJSON(time.Now(), 12.5)
	rr := post(h, "/api/usage", body, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}
	var up struct {
		OK      int `json:"ok"`
		Devices int `json:"devices"`
	}
	json.Unmarshal(rr.Body.Bytes(), &up)
	if up.OK != 1 || up.Devices != 1 {
		t.Errorf("响应 = %+v，期望 ok=1 devices=1", up)
	}

	rr = get(h, "/api/agent/usage", bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("读取 = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		TS       int64 `json:"ts"`
		FiveHour *struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"five_hour"`
		Limits json.RawMessage `json:"limits"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.FiveHour == nil || out.FiveHour.UsedPercentage != 12.5 {
		t.Errorf("five_hour = %+v", out.FiveHour)
	}
	if out.TS == 0 {
		t.Error("ts 没给")
	}
	if len(out.Limits) == 0 || string(out.Limits) == "null" {
		t.Errorf("limits 没原样返回：%s", out.Limits)
	}
}

func TestUsagePostRejectsInvalidReport(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := post(h, "/api/usage", `{}`, bearer(agentTok)); rr.Code != 400 {
		t.Errorf("空报告 = %d，期望 400", rr.Code)
	}
	if rr := post(h, "/api/usage", `not json`, bearer(agentTok)); rr.Code != 400 {
		t.Errorf("畸形 JSON = %d，期望 400", rr.Code)
	}
}

func TestUsageRequiresAgent(t *testing.T) {
	h, _ := newTestAPI(t)
	if rr := post(h, "/api/usage", reportJSON(time.Now(), 1), nil); rr.Code != 401 {
		t.Errorf("无令牌 = %d，期望 401", rr.Code)
	}
}

func TestAgentLogout(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := post(h, "/api/agent/logout", `{}`, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("注销 = %d %s", rr.Code, rr.Body)
	}
	if rr := post(h, "/api/usage", reportJSON(time.Now(), 1), bearer(agentTok)); rr.Code != 401 {
		t.Errorf("注销后仍能上报 = %d，期望 401", rr.Code)
	}
}

// agent 令牌不能碰设备端点；设备密钥不能碰 agent 端点。
func TestRoleIsolation(t *testing.T) {
	h, _ := newTestAPI(t)
	devTok, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := post(h, "/api/device/pair-code", `{}`, bearer(agentTok)); rr.Code != 401 {
		t.Errorf("agent 令牌换配对码 = %d，期望 401", rr.Code)
	}
	if rr := get(h, "/api/agent/devices", bearer(devTok)); rr.Code != 401 {
		t.Errorf("device 密钥列设备 = %d，期望 401", rr.Code)
	}
	if rr := post(h, "/api/agent/unpair", `{"dev":"f412fa87c9e0"}`, bearer(devTok)); rr.Code != 401 {
		t.Errorf("device 密钥解绑 = %d，期望 401", rr.Code)
	}
}
