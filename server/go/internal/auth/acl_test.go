package auth

import "testing"

// T7 没有 App 角色：设备只能写自己的 status、读自己的 usage 与 pair，
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
		{"设备不能伪造用量", dev, "printer/aaa/usage", true, false},
		{"设备不能伪造配对", dev, "printer/aaa/pair", true, false},
		{"设备不能收别人的用量", dev, "printer/bbb/usage", false, false},
		{"设备不能收别人的配对", dev, "printer/bbb/pair", false, false},
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

		{"agent 不能推用量", agent, "printer/aaa/usage", true, false},
		{"agent 不能订阅状态", agent, "printer/aaa/status", false, false},
		{"agent 不能订阅配对", agent, "printer/aaa/pair", false, false},

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
