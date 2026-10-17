package auth

import "testing"

// T7 没有 App 角色。T18 起设备还要写自己的 ack（下行指令回执），
// agent 不连 MQTT。
func TestACLMatrix(t *testing.T) {
	dev := Identity{Dev: "aaa", Role: RoleDevice}
	pending := Identity{Dev: "aaa", Role: RoleDevice, Pending: true}
	agent := Identity{UserID: "u1", Role: RoleAgent}

	cases := []struct {
		name  string
		id    Identity
		topic string
		write bool
		want  bool
	}{
		{"设备发状态", dev, "printer/aaa/status", true, true},
		{"设备收用量", dev, "printer/aaa/usage", false, true},
		{"设备收配对通知", dev, "printer/aaa/pair", false, true},
		{"设备收帧版本", dev, "printer/aaa/frame", false, true},
		{"设备不能伪造用量", dev, "printer/aaa/usage", true, false},
		{"设备不能伪造配对", dev, "printer/aaa/pair", true, false},
		{"设备不能推帧版本", dev, "printer/aaa/frame", true, false},
		{"设备不能收别人的用量", dev, "printer/bbb/usage", false, false},
		{"设备不能收别人的配对", dev, "printer/bbb/pair", false, false},
		{"设备不能收别人的帧版本", dev, "printer/bbb/frame", false, false},
		{"设备不能订阅自己的状态", dev, "printer/aaa/status", false, false},
		{"设备不能碰别人", dev, "printer/bbb/status", true, false},
		{"设备不能用通配符", dev, "printer/+/status", false, false},

		// pending 密钥只代表「知道 MAC」，不是主人。
		{"pending 能读配对", pending, "printer/aaa/pair", false, true},
		{"pending 能发心跳", pending, "printer/aaa/status", true, true},
		{"pending 不能读用量", pending, "printer/aaa/usage", false, false},
		{"pending 不能读帧版本", pending, "printer/aaa/frame", false, false},
		{"pending 不能伪造配对", pending, "printer/aaa/pair", true, false},
		{"pending 不能读别人的配对", pending, "printer/bbb/pair", false, false},
		// 用受害者 MAC 注册出来的 pending 密钥 dev 完全相同，而 tracker 只
		// 比对 dev——放它写 ack 就能替受害设备伪造回执。解绑后无法 ACK 的
		// 旧指令由服务端的 CancelDevice 结案，不靠放开 ACL。
		{"pending 不能回 ACK", pending, "printer/aaa/ack", true, false},

		// T18：下行指令回执。设备写自己的 ack，不能写别人的、也不能订阅
		// 任何人的——ack 是单向上行，能订阅就能看到别人设备的执行情况。
		{"设备回 ACK", dev, "printer/aaa/ack", true, true},
		{"设备不能替别人回 ACK", dev, "printer/bbb/ack", true, false},
		{"设备不能订阅 ACK", dev, "printer/aaa/ack", false, false},
		{"agent 不能回 ACK", agent, "printer/aaa/ack", true, false},

		// T13：帧版本通知。和 usage 一样是「已配对才能读」。
		{"设备收帧版本通知", dev, "printer/aaa/frame", false, true},
		{"设备不能伪造帧版本通知", dev, "printer/aaa/frame", true, false},
		{"设备不能收别人的帧版本通知", dev, "printer/bbb/frame", false, false},
		{"agent 不能订阅帧版本通知", agent, "printer/aaa/frame", false, false},

		{"agent 不能推用量", agent, "printer/aaa/usage", true, false},
		{"agent 不能订阅状态", agent, "printer/aaa/status", false, false},
		{"agent 不能订阅配对", agent, "printer/aaa/pair", false, false},
		{"agent 不能订阅帧版本", agent, "printer/aaa/frame", false, false},

		{"越界前缀", dev, "printer/aaa", false, false},
		{"多余层级", dev, "printer/aaa/status/x", false, false},
		{"完全无关的 topic", dev, "$SYS/broker/uptime", false, false},
	}
	for _, c := range cases {
		if got := ACLAllowed(c.id, c.topic, c.write); got != c.want {
			t.Errorf("%s: ACLAllowed(%+v, %q, write=%v) = %v，期望 %v",
				c.name, c.id, c.topic, c.write, got, c.want)
		}
	}
}
