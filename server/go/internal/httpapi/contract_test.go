package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// 设备用 esp_http_client，**不解 chunked 响应体**。
// 所有 JSON 响应都必须带 Content-Length（SERVER-REQUIREMENTS 3.3）。
func TestAllJSONResponsesHaveContentLength(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")

	checks := []*httptest.ResponseRecorder{
		get(h, "/api/status", nil),                                   // 401
		post(h, "/api/auth/sms", `{"phone":"12345"}`, nil),           // 400
		post(h, "/api/auth/sms", `{"phone":"13900001111"}`, nil),     // 200
		post(h, "/api/device/enroll", `{"dev":"zz"}`, bearer(token)), // 400
	}
	for i, rr := range checks {
		cl := rr.Header().Get("Content-Length")
		if cl == "" {
			t.Errorf("第 %d 个响应（%d）没有 Content-Length——设备解不了 chunked", i+1, rr.Code)
			continue
		}
		n, err := strconv.Atoi(cl)
		if err != nil || n != rr.Body.Len() {
			t.Errorf("第 %d 个响应 Content-Length=%q 与实际 %d 字节不符",
				i+1, cl, rr.Body.Len())
		}
		if rr.Header().Get("Transfer-Encoding") != "" {
			t.Errorf("第 %d 个响应用了 Transfer-Encoding", i+1)
		}
	}
}

// 任何路径的 POST 都必须读完请求体，即使要返回错误
// （SERVER-REQUIREMENTS 3.1）。不读会让 HTTP/1.1 长连接失步：
// 设备发完几 KB 卡住等响应，34 秒后 MQTT 跟着写超时，最后设备重启。
func TestErrorResponsesDrainRequestBody(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, token, "f412fa87c9e0")

	big := bytes.Repeat([]byte("x"), 7<<10)

	cases := []struct {
		name string
		req  *http.Request
		hdr  map[string]string
	}{
		{"未知路径", httptest.NewRequest("POST", "/api/nope", bytes.NewReader(big)), nil},
		{"未授权的用量上报",
			httptest.NewRequest("POST", "/api/usage", bytes.NewReader(big)), nil},
		{"app 令牌不能上报用量",
			httptest.NewRequest("POST", "/api/usage", bytes.NewReader(big)),
			bearer(token)},
		{"device 密钥不能 enroll",
			httptest.NewRequest("POST", "/api/device/enroll", bytes.NewReader(big)),
			bearer(devKey)},
	}
	for _, c := range cases {
		rr := do(h, c.req, c.hdr)
		left, _ := io_ReadAll(c.req.Body)
		if len(left) != 0 {
			t.Errorf("%s（%d）：请求体还剩 %d 字节没读——连接会失步",
				c.name, rr.Code, len(left))
		}
	}
}
