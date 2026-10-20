package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func webPair(h http.Handler, dev, code string) *httptest.ResponseRecorder {
	return post(h, "/api/web/pair", `{"dev":"`+dev+`","code":"`+code+`"}`,
		map[string]string{"Content-Type": "application/json"})
}

func redeem(h http.Handler, link string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"link": link})
	return post(h, "/api/agent/redeem", string(b), map[string]string{"Content-Type": "application/json"})
}

func mustWebLink(t *testing.T, h http.Handler, dev, code string) string {
	t.Helper()
	rr := webPair(h, dev, code)
	if rr.Code != 200 {
		t.Fatalf("web pair = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Link      string `json:"link"`
		ExpiresIn int    `json:"expires_in"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.ExpiresIn != 900 || !strings.Contains(out.Link, "/r/") {
		t.Fatalf("响应异常：%s", rr.Body)
	}
	if strings.Contains(rr.Body.String(), "token") {
		t.Fatalf("手机端响应不该含令牌：%s", rr.Body)
	}
	return out.Link
}

func TestWebPairThenRedeemSuccess(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	link := mustWebLink(t, h, "f412fa87c9e0", code)

	// 提交网页码不应提前绑定设备。
	if _, bound, _ := dep.store.OwnerOfDevice("f412fa87c9e0"); bound {
		t.Fatal("web pair 阶段不该绑定设备")
	}
	rr := redeem(h, link)
	if rr.Code != 200 {
		t.Fatalf("redeem = %d %s", rr.Code, rr.Body)
	}
	var m map[string]string
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m["dev"] != "f412fa87c9e0" || m["user_id"] == "" || m["token"] == "" {
		t.Fatalf("redeem 响应 %v", m)
	}
	id, err := dep.v.Verify(m["token"])
	if err != nil || id.Role != auth.RoleAgent || id.UserID != m["user_id"] {
		t.Errorf("令牌身份 %+v %v", id, err)
	}
	if devs := agentDevices(t, h, m["token"]); len(devs) != 1 || devs[0].Dev != m["dev"] {
		t.Errorf("设备列表 %+v", devs)
	}
	var note struct {
		Paired bool `json:"paired"`
	}
	json.Unmarshal(dep.pub.pair[m["dev"]], &note)
	if !note.Paired {
		t.Errorf("设备没收到配对通知：%s", dep.pub.pair[m["dev"]])
	}
}

func TestRedeemSingleUse(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	link := mustWebLink(t, h, "f412fa87c9e0", code)
	if rr := redeem(h, link); rr.Code != 200 {
		t.Fatalf("首次 = %d", rr.Code)
	}
	rr := redeem(h, link)
	if rr.Code != 404 || strings.Contains(rr.Body.String(), "token") {
		t.Fatalf("重复兑换 = %d %s", rr.Code, rr.Body)
	}
}

func TestRedeemExpired(t *testing.T) {
	h, dep := newTestAPI(t)
	register(t, h, "f412fa87c9e0")
	pc, ok, err := dep.store.PairCodeForDev("f412fa87c9e0")
	if err != nil || !ok {
		t.Fatal("查不到配对码")
	}
	raw := make([]byte, 32)
	raw[0] = 7
	if err := dep.store.PutPairRedeem(store.PairRedeem{
		LinkHash: linkHash(raw), Dev: "f412fa87c9e0", KeyID: pc.KeyID,
		Expires: time.Now().Add(-time.Second).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	link := "https://x.example/r/" + base64.RawURLEncoding.EncodeToString(raw)
	if rr := redeem(h, link); rr.Code != 404 {
		t.Fatalf("过期链接 = %d", rr.Code)
	}
}

func TestRedeemBadLinks(t *testing.T) {
	h, _ := newTestAPI(t)
	zero := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, l := range []string{"", "nonsense", "https://x/r/", "https://x/r/AAAA",
		"https://x/other/" + zero, "https://x/r/" + zero} {
		rr := redeem(h, l)
		if rr.Code != 404 || !strings.Contains(rr.Body.String(), "bad link") {
			t.Errorf("link %q = %d %s", l, rr.Code, rr.Body)
		}
	}
}

func TestWebPairWrongCode(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	if rr := webPair(h, "f412fa87c9e0", wrong); rr.Code != 404 {
		t.Fatalf("错码 = %d", rr.Code)
	}
	// 错码不烧掉真码。
	mustWebLink(t, h, "f412fa87c9e0", code)
}

func TestWebPairDevMismatchKeepsCode(t *testing.T) {
	h, _ := newTestAPI(t)
	_, codeA := register(t, h, "f412fa87c9e0")
	register(t, h, "aabbccddeeff")
	// 拿 A 的码配 B：按错码处理，A 的码还能用。
	if rr := webPair(h, "aabbccddeeff", codeA); rr.Code != 404 {
		t.Fatalf("dev 不匹配 = %d", rr.Code)
	}
	for _, bad := range []string{"", "F412FA87C9E0", "f412fa87c9e", "zzzzzzzzzzzz"} {
		if rr := webPair(h, bad, codeA); rr.Code != 400 {
			t.Errorf("dev %q = %d", bad, rr.Code)
		}
	}
	mustWebLink(t, h, "f412fa87c9e0", codeA)
}

func TestWebPairBoundDeviceRejectedCodeKept(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code1 := register(t, h, "f412fa87c9e0")
	agentA, _, _ := mustPair(t, h, code1, "")
	_, code2 := register(t, h, "f412fa87c9e0") // 擦 NVS 重注册
	if rr := webPair(h, "f412fa87c9e0", code2); rr.Code != 409 {
		t.Fatalf("已绑定 = %d", rr.Code)
	}
	// 原主人 agent 仍能用这个码。
	mustPair(t, h, code2, agentA)
}

func TestWebPairCodeSingleUse(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	mustWebLink(t, h, "f412fa87c9e0", code)
	if rr := webPair(h, "f412fa87c9e0", code); rr.Code != 404 {
		t.Fatalf("同码二次提交 = %d", rr.Code)
	}
	if rr := tryPair(h, code, ""); rr.Code != 404 {
		t.Fatalf("agent/pair 再用同码 = %d", rr.Code)
	}
}

func TestWebPairConcurrentSameCode(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = webPair(h, "f412fa87c9e0", code).Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		} else if c != 404 {
			t.Errorf("意外状态 %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("成功 %d 次，期望 1：%v", ok, codes)
	}
}

func TestRedeemConcurrentSameLink(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	link := mustWebLink(t, h, "f412fa87c9e0", code)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = redeem(h, link).Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("成功 %d 次：%v", ok, codes)
	}
}

func TestWebPairRateLimit(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	for i := 0; i < 10; i++ {
		if rr := webPair(h, "f412fa87c9e0", wrong); rr.Code != 404 {
			t.Fatalf("第 %d 次 = %d", i, rr.Code)
		}
	}
	if rr := webPair(h, "f412fa87c9e0", code); rr.Code != 429 {
		t.Fatalf("超限后 = %d，期望 429", rr.Code)
	}
}

func TestRedeemRateLimit(t *testing.T) {
	h, _ := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	good := mustWebLink(t, h, "f412fa87c9e0", code)
	bad := "https://x/r/" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for i := 0; i < 10; i++ {
		if rr := redeem(h, bad); rr.Code != 404 {
			t.Fatalf("第 %d 次 = %d", i, rr.Code)
		}
	}
	if rr := redeem(h, good); rr.Code != 429 {
		t.Fatalf("超限后真链接 = %d，期望 429", rr.Code)
	}
}

func TestRedeemStoresOnlyHash(t *testing.T) {
	h, dep := newTestAPI(t)
	_, code := register(t, h, "f412fa87c9e0")
	link := mustWebLink(t, h, "f412fa87c9e0", code)
	secret := link[strings.LastIndex(link, "/")+1:]
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	if len(raw) != 32 {
		t.Fatalf("链接熵 %d 字节", len(raw))
	}
	sum := sha256.Sum256(raw)
	rd, ok, _ := dep.store.ConsumePairRedeem(hex.EncodeToString(sum[:]), time.Now().Unix())
	if !ok || strings.Contains(rd.LinkHash, secret) {
		t.Fatalf("库里应按哈希查到：%+v", rd)
	}
	if time.Unix(rd.Expires, 0).Sub(time.Now()) > 15*time.Minute+time.Second {
		t.Errorf("TTL 超过 15 分钟")
	}
}

func TestWebPairPageHeaders(t *testing.T) {
	h, _ := newTestAPI(t)
	for _, p := range []string{"/p?d=f412fa87c9e0", "/p/app.js", "/p/app.css"} {
		rr := get(h, p, nil)
		if rr.Code != 200 {
			t.Fatalf("%s = %d", p, rr.Code)
		}
		hd := rr.Header()
		if hd.Get("Cache-Control") != "no-store" || hd.Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(hd.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s 头不对：%v", p, hd)
		}
		body := rr.Body.String()
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Errorf("%s 含外部地址", p)
		}
	}
}
