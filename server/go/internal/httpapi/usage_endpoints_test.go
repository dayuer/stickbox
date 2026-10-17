package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func reportJSON(now time.Time, pct float64) string {
	return fmt.Sprintf(`{"five_hour":{"used_percentage":%g,"resets_at":%d},`+
		`"seven_day":{"used_percentage":%g,"resets_at":%d},`+
		`"limits":[{"kind":"weekly_all","percent":%g}]}`,
		pct, now.Add(time.Hour).Unix(), pct/2, now.Add(48*time.Hour).Unix(), pct/2)
}

// agent 登录要求账号已存在：没有账号就推不到任何屏幕，不如直接 403。
func TestAgentLoginRequiresExistingAccount(t *testing.T) {
	h, dep := newTestAPI(t)
	if rr := post(h, "/api/auth/sms", `{"phone":"13800008888"}`, nil); rr.Code != 200 {
		t.Fatalf("发码 = %d %s", rr.Code, rr.Body)
	}
	rr := post(h, "/api/agent/login",
		`{"phone":"13800008888","code":"`+dep.sender.last+`","name":"mac"}`, nil)
	if rr.Code != 403 {
		t.Fatalf("无账号登录 = %d，期望 403：%s", rr.Code, rr.Body)
	}
}

func TestAgentLoginIssuesAgentToken(t *testing.T) {
	h, dep := newTestAPI(t)
	login(t, h, dep, "13800008888")
	tok := agentLogin(t, h, dep, "13800008888")

	id, err := dep.v.Verify(tok)
	if err != nil {
		t.Fatalf("agent 令牌不可用：%v", err)
	}
	if string(id.Role) != "agent" {
		t.Errorf("角色 = %q，期望 agent", id.Role)
	}
	if id.Dev != "" {
		t.Errorf("agent 不该有 dev，拿到 %q", id.Dev)
	}
}

// agent 上报 → 落库 → App 读取。
func TestUsagePostAndAppRead(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	agentTok := agentLogin(t, h, dep, "13800008888")

	if rr := get(h, "/api/usage", bearer(appTok)); rr.Code != 404 {
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
	if up.OK != 1 || up.Devices != 0 {
		t.Errorf("响应 = %+v，期望 ok=1 devices=0", up)
	}

	rr = get(h, "/api/usage", bearer(appTok))
	if rr.Code != 200 {
		t.Fatalf("App 读取 = %d %s", rr.Code, rr.Body)
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
	h, dep := newTestAPI(t)
	login(t, h, dep, "13800008888")
	agentTok := agentLogin(t, h, dep, "13800008888")

	if rr := post(h, "/api/usage", `{}`, bearer(agentTok)); rr.Code != 400 {
		t.Errorf("空报告 = %d，期望 400", rr.Code)
	}
	if rr := post(h, "/api/usage", `not json`, bearer(agentTok)); rr.Code != 400 {
		t.Errorf("畸形 JSON = %d，期望 400", rr.Code)
	}
}

// App 能列出并吊销该账号下的 agent，吊销后令牌立即失效。
func TestAgentsListAndRevoke(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	agentTok := agentLogin(t, h, dep, "13800008888")

	rr := get(h, "/api/agents", bearer(appTok))
	if rr.Code != 200 {
		t.Fatalf("列表 = %d %s", rr.Code, rr.Body)
	}
	var list struct {
		Agents []struct {
			KeyID string `json:"key_id"`
			Name  string `json:"name"`
		} `json:"agents"`
	}
	json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Agents) != 1 || list.Agents[0].Name != "mac" {
		t.Fatalf("agents = %+v", list.Agents)
	}

	rr = post(h, "/api/agent/"+list.Agents[0].KeyID+"/revoke", `{}`, bearer(appTok))
	if rr.Code != 200 {
		t.Fatalf("吊销 = %d %s", rr.Code, rr.Body)
	}
	if rr := post(h, "/api/usage", reportJSON(time.Now(), 1), bearer(agentTok)); rr.Code != 401 {
		t.Errorf("吊销后仍能上报 = %d，期望 401", rr.Code)
	}
}

// agent 自己注销。
func TestAgentLogout(t *testing.T) {
	h, dep := newTestAPI(t)
	login(t, h, dep, "13800008888")
	agentTok := agentLogin(t, h, dep, "13800008888")

	if rr := post(h, "/api/agent/logout", `{}`, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("注销 = %d %s", rr.Code, rr.Body)
	}
	if rr := post(h, "/api/usage", reportJSON(time.Now(), 1), bearer(agentTok)); rr.Code != 401 {
		t.Errorf("注销后仍能上报 = %d，期望 401", rr.Code)
	}
}

// 越权边界：agent 只能上报用量、访问自己的 agent 端点，
// 不能读账号数据、不能管设备、不能查状态。
func TestAgentCannotUseAppOrDeviceEndpoints(t *testing.T) {
	h, dep := newTestAPI(t)
	appTok := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, appTok, "f412fa87c9e0")
	agentTok := agentLogin(t, h, dep, "13800008888")

	cases := []struct {
		name string
		rr   func() *httptest.ResponseRecorder
	}{
		{"GET /api/usage", func() *httptest.ResponseRecorder {
			return get(h, "/api/usage", bearer(agentTok))
		}},
		{"GET /api/devices", func() *httptest.ResponseRecorder {
			return get(h, "/api/devices", bearer(agentTok))
		}},
		{"GET /api/agents", func() *httptest.ResponseRecorder {
			return get(h, "/api/agents", bearer(agentTok))
		}},
		{"GET /api/status", func() *httptest.ResponseRecorder {
			return get(h, "/api/status", bearer(agentTok))
		}},
		{"POST /api/device/enroll", func() *httptest.ResponseRecorder {
			return post(h, "/api/device/enroll", `{"dev":"aaaaaaaaaaaa"}`, bearer(agentTok))
		}},
		{"POST /api/device/{dev}/unbind", func() *httptest.ResponseRecorder {
			return post(h, "/api/device/f412fa87c9e0/unbind", `{}`, bearer(agentTok))
		}},
	}
	for _, c := range cases {
		if rr := c.rr(); rr.Code != 401 {
			t.Errorf("%s = %d，期望 401", c.name, rr.Code)
		}
	}

	// agent 令牌也不能拿来连 MQTT；这里只验证 device 密钥拿不到 agent 端点。
	if rr := post(h, "/api/usage", reportJSON(time.Now(), 1), bearer(devKey)); rr.Code != 401 {
		t.Errorf("device 密钥上报用量 = %d，期望 401", rr.Code)
	}
}
