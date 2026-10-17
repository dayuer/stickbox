package httpapi

import (
	"encoding/json"
	"testing"
)

const dataBody = `{"date":"2026-10-17","tz":"Asia/Shanghai",` +
	`"gmv":{"value":146000,"goal":200000},` +
	`"orders":{"value":420,"goal":400},` +
	`"card_revenue":{"value":8000,"goal":10000}}`

// agent 上报 → 落库 → agent 读回。
func TestDataPostAndAgentRead(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	if rr := get(h, "/api/agent/data", bearer(agentTok)); rr.Code != 404 {
		t.Fatalf("还没上报时 = %d，期望 404", rr.Code)
	}
	rr := post(h, "/api/data", dataBody, bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("上报 = %d %s", rr.Code, rr.Body)
	}
	var up struct{ OK int }
	json.Unmarshal(rr.Body.Bytes(), &up)
	if up.OK != 1 {
		t.Errorf("响应 = %s，期望 ok=1", rr.Body)
	}

	rr = get(h, "/api/agent/data", bearer(agentTok))
	if rr.Code != 200 {
		t.Fatalf("读回 = %d %s", rr.Code, rr.Body)
	}
	var got struct {
		TS   int64  `json:"ts"`
		Date string `json:"date"`
		TZ   string `json:"tz"`
		GMV  *struct {
			Value float64 `json:"value"`
			Goal  float64 `json:"goal"`
		} `json:"gmv"`
		CardRevenue *struct {
			Value float64 `json:"value"`
		} `json:"card_revenue"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Date != "2026-10-17" || got.TZ != "Asia/Shanghai" || got.TS == 0 {
		t.Errorf("读回元数据不对: %s", rr.Body)
	}
	if got.GMV == nil || got.GMV.Value != 146000 || got.GMV.Goal != 200000 {
		t.Errorf("成交额不对: %s", rr.Body)
	}
	// 权益卡收入现在不上屏，但必须原样存下来——换 Show 就能顶上去。
	if got.CardRevenue == nil || got.CardRevenue.Value != 8000 {
		t.Errorf("权益卡收入没存下来: %s", rr.Body)
	}
}

func TestDataPostRejectsBadInput(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	cases := map[string]string{
		"不是 JSON":  `not json`,
		"空对象":      `{}`,
		"缺日期":      `{"gmv":{"value":1,"goal":2}}`,
		"日期格式错":    `{"date":"2026/10/17","gmv":{"value":1,"goal":2}}`,
		"一个指标都没有":  `{"date":"2026-10-17"}`,
		"负值":       `{"date":"2026-10-17","gmv":{"value":-1,"goal":2}}`,
		"实际值超目标百倍": `{"date":"2026-10-17","gmv":{"value":1000,"goal":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if rr := post(h, "/api/data", body, bearer(agentTok)); rr.Code != 400 {
				t.Errorf("= %d %s，期望 400", rr.Code, rr.Body)
			}
		})
	}
}

// 目标为 0 是合法的（只是不上屏），不能当成非法数据拒掉整条上报。
func TestDataPostAcceptsZeroGoal(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, _, _ := mustPair(t, h, code, "")

	body := `{"date":"2026-10-17","gmv":{"value":146000,"goal":0},` +
		`"orders":{"value":420,"goal":400}}`
	if rr := post(h, "/api/data", body, bearer(agentTok)); rr.Code != 200 {
		t.Fatalf("= %d %s，期望 200", rr.Code, rr.Body)
	}
}

func TestDataEndpointsRequireAgent(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	agentTok, devTok, _ := mustPair(t, h, code, "")

	if rr := post(h, "/api/data", dataBody, nil); rr.Code != 401 {
		t.Errorf("无令牌上报 = %d，期望 401", rr.Code)
	}
	// 设备令牌不能写经营数据：屏幕是只读的那一端。
	if rr := post(h, "/api/data", dataBody, bearer(devTok)); rr.Code != 401 {
		t.Errorf("设备令牌上报 = %d，期望 401", rr.Code)
	}
	if rr := get(h, "/api/agent/data", bearer(devTok)); rr.Code != 401 {
		t.Errorf("设备令牌读取 = %d，期望 401", rr.Code)
	}
	if rr := post(h, "/api/data", dataBody, bearer(agentTok)); rr.Code != 200 {
		t.Errorf("agent 令牌上报 = %d %s，期望 200", rr.Code, rr.Body)
	}
}

// 两个用户的数据不能互相看见。
func TestDataIsolatedPerUser(t *testing.T) {
	h, _ := newTestAPI(t)
	_, codeA := register(t, h, "f412fa87c9e0")
	agentA, _, _ := mustPair(t, h, codeA, "")
	_, codeB := register(t, h, "958f6379cdfe")
	agentB, _, _ := mustPair(t, h, codeB, "")

	if rr := post(h, "/api/data", dataBody, bearer(agentA)); rr.Code != 200 {
		t.Fatalf("A 上报 = %d %s", rr.Code, rr.Body)
	}
	if rr := get(h, "/api/agent/data", bearer(agentB)); rr.Code != 404 {
		t.Errorf("B 读到了 A 的数据 = %d %s", rr.Code, rr.Body)
	}
}
