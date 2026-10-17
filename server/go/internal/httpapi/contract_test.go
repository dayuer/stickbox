package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// 设备用 esp_http_client，**不解 chunked 响应体**。
// 所有 JSON 响应都必须带 Content-Length。
func TestAllJSONResponsesHaveContentLength(t *testing.T) {
	h, _ := newTestAPI(t)

	checks := []*httptest.ResponseRecorder{
		get(h, "/api/agent/devices", nil),                              // 401
		post(h, "/api/device/register", `{"dev":"zz"}`, nil),           // 400
		post(h, "/api/device/register", `{"dev":"f412fa87c9e0"}`, nil), // 200
		post(h, "/api/agent/pair", `{"code":"123"}`, nil),              // 400
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

// 任何路径的 POST 都必须读完请求体，即使要返回错误。
// 不读会让 HTTP/1.1 长连接失步，设备卡住直到超时重启。
func TestErrorResponsesDrainRequestBody(t *testing.T) {
	h, _ := newTestAPI(t)
	big := bytes.Repeat([]byte("x"), 7<<10)

	cases := []struct {
		name string
		req  *http.Request
		hdr  map[string]string
	}{
		{"未知路径", httptest.NewRequest("POST", "/api/nope", bytes.NewReader(big)), nil},
		{"没有令牌上报用量",
			httptest.NewRequest("POST", "/api/usage", bytes.NewReader(big)), nil},
		{"配对请求体过大",
			httptest.NewRequest("POST", "/api/agent/pair", bytes.NewReader(big)), nil},
		{"注册请求体过大",
			httptest.NewRequest("POST", "/api/device/register", bytes.NewReader(big)), nil},
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
