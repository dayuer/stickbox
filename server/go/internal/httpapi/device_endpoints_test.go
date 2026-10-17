package httpapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnrollIssuesDeviceKey(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")

	rr := post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0","name":"工位"}`, bearer(token))
	if rr.Code != 200 {
		t.Fatalf("enroll = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		DeviceKey string `json:"device_key"`
		Reset     bool   `json:"reset"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.DeviceKey == "" || out.Reset {
		t.Fatalf("首次 enroll 响应异常：%s", rr.Body)
	}
	id, err := dep.v.Verify(out.DeviceKey)
	if err != nil || id.Dev != "f412fa87c9e0" {
		t.Errorf("签发的密钥不可用：%+v err=%v", id, err)
	}
}

func TestEnrollRejectsBadDev(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	// 空串不在里面：不带 dev 是合法的，表示签发一把待认领的密钥。
	for _, dev := range []string{"XYZ", "F412FA87C9E0", "f412fa87c9e", "f412fa87c9e0f"} {
		rr := post(h, "/api/device/enroll", `{"dev":"`+dev+`"}`, bearer(token))
		if rr.Code != 400 {
			t.Errorf("dev=%q → %d，期望 400", dev, rr.Code)
		}
	}
}

// 抢绑防护：别人的设备不能被绑走，否则是条现成的 DoS。
func TestEnrollRejectsOtherUsersDevice(t *testing.T) {
	h, dep := newTestAPI(t)
	tokenA := login(t, h, dep, "13800008888")
	enroll(t, h, tokenA, "f412fa87c9e0")

	dep.clock = dep.clock.Add(2 * time.Minute)
	tokenB := login(t, h, dep, "13900009999")
	rr := post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0"}`, bearer(tokenB))
	if rr.Code != 409 {
		t.Errorf("抢绑 = %d，期望 409", rr.Code)
	}
}

// 重复 enroll 自己的设备 = 重置：旧密钥吊销，新密钥生效。
func TestEnrollTwiceIsReset(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	first := enroll(t, h, token, "f412fa87c9e0")
	dep.v.Verify(first) // 先进缓存，确保吊销时缓存也被清

	rr := post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0"}`, bearer(token))
	var out struct {
		DeviceKey string `json:"device_key"`
		Reset     bool   `json:"reset"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Reset {
		t.Error("重复 enroll 应标记为 reset")
	}
	if out.DeviceKey == first {
		t.Fatal("重置后密钥没变")
	}
	if _, err := dep.v.Verify(first); err == nil {
		t.Error("旧设备密钥仍可用——重置没吊销它")
	}
	if _, err := dep.v.Verify(out.DeviceKey); err != nil {
		t.Errorf("新密钥不可用：%v", err)
	}
}

func TestUnbindReleasesDevice(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	enroll(t, h, token, "f412fa87c9e0")

	if rr := post(h, "/api/device/f412fa87c9e0/unbind", `{}`, bearer(token)); rr.Code != 200 {
		t.Fatalf("解绑 = %d %s", rr.Code, rr.Body)
	}
	if _, ok, _ := dep.store.OwnerOfDevice("f412fa87c9e0"); ok {
		t.Error("解绑后设备仍有归属")
	}
	// 幂等
	if rr := post(h, "/api/device/f412fa87c9e0/unbind", `{}`, bearer(token)); rr.Code != 200 {
		t.Error("重复解绑应当幂等")
	}
	// 解绑后别人可以绑
	dep.clock = dep.clock.Add(2 * time.Minute)
	other := login(t, h, dep, "13900009999")
	if rr := post(h, "/api/device/enroll", `{"dev":"f412fa87c9e0"}`, bearer(other)); rr.Code != 200 {
		t.Errorf("解绑后新用户 enroll = %d", rr.Code)
	}
}

func TestStatusScopedToOwnDevice(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	enroll(t, h, token, "f412fa87c9e0")

	rr := get(h, "/api/status", map[string]string{
		"Authorization": "Bearer " + token, "X-Device": "f412fa87c9e0"})
	if rr.Code != 200 {
		t.Fatalf("= %d %s", rr.Code, rr.Body)
	}
	rr = get(h, "/api/status", map[string]string{
		"Authorization": "Bearer " + token, "X-Device": "aaaaaaaaaaaa"})
	if rr.Code != 403 {
		t.Errorf("查别人的设备 = %d，期望 403", rr.Code)
	}
}

// 在线判据就是最近一次 MQTT 心跳；没发过心跳就是离线。
func TestStatusReportsHeartbeatPresence(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")
	devKey := enroll(t, h, token, "f412fa87c9e0")

	getStatus := func(tok string) bool {
		rr := get(h, "/api/status", map[string]string{
			"Authorization": "Bearer " + tok, "X-Device": "f412fa87c9e0"})
		if rr.Code != 200 {
			t.Fatalf("status = %d %s", rr.Code, rr.Body)
		}
		var out struct {
			Device struct {
				Online bool `json:"online"`
			} `json:"device"`
		}
		json.Unmarshal(rr.Body.Bytes(), &out)
		return out.Device.Online
	}

	if getStatus(token) {
		t.Error("没发过心跳却显示在线")
	}
	dep.reg.Seen("f412fa87c9e0")
	if !getStatus(token) {
		t.Error("收到心跳后仍显示离线")
	}
	// 设备角色查自己，不依赖 X-Device
	rr := get(h, "/api/status", bearer(devKey))
	if rr.Code != 200 {
		t.Fatalf("device 查状态 = %d %s", rr.Code, rr.Body)
	}
}

// 不带 dev 时签发一把待认领的密钥。
//
// 这是为了让 App 在**还没连上设备热点**的时候就能拿到密钥：连一次热点写
// 进去就完事，不用先读 MAC 再断开去申请。服务端本来就能从设备自报的
// username 学到 MAC，让 App 先去问设备要是多余的。
func TestEnrollWithoutDevIssuesUnclaimedKey(t *testing.T) {
	h, dep := newTestAPI(t)
	token := login(t, h, dep, "13800008888")

	rr := post(h, "/api/device/enroll", `{"name":"还没连上的设备"}`, bearer(token))
	if rr.Code != 200 {
		t.Fatalf("不带 dev 的 enroll = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		DeviceKey string `json:"device_key"`
		Dev       string `json:"dev"`
		Reset     bool   `json:"reset"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.DeviceKey == "" {
		t.Fatal("没签发密钥")
	}
	if out.Dev != "" {
		t.Errorf("待认领的密钥不该带 dev，拿到 %q", out.Dev)
	}
	if out.Reset {
		t.Error("待认领的密钥没有「重置」一说")
	}

	// 密钥本身可用，但还没绑定设备——绑定发生在设备首次连 MQTT 时。
	id, err := dep.v.Verify(out.DeviceKey)
	if err != nil {
		t.Fatalf("签发的密钥不可用：%v", err)
	}
	if id.Dev != "" {
		t.Errorf("此时不该有 dev，拿到 %q", id.Dev)
	}
	if string(id.Role) != "device" {
		t.Errorf("角色 = %q，期望 device", id.Role)
	}
}
