package auth

import "testing"

// fakeMembership: u1 名下有 aaa 和 ccc，没有 bbb。
type fakeMembership struct{}

func (fakeMembership) HasDevice(userID, dev string) bool {
	return userID == "u1" && (dev == "aaa" || dev == "ccc")
}

// 这张矩阵是安全边界，改动必须先改这里。
//
// 打印退役后设备只剩 status（写，心跳）和 usage（读，用量）两个主题；
// job / profile / ident / cmd 的准入已随打印链路删除。
func TestACLMatrix(t *testing.T) {
	dev := Identity{Dev: "aaa", Role: RoleDevice}
	app := Identity{UserID: "u1", Role: RoleApp}
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
		{"设备不能伪造用量", dev, "printer/aaa/usage", true, false},
		{"设备不能收别人的用量", dev, "printer/bbb/usage", false, false},
		{"设备不能订阅自己的状态", dev, "printer/aaa/status", false, false},
		{"设备不能碰别人", dev, "printer/bbb/status", true, false},
		{"设备不能用通配符", dev, "printer/+/status", false, false},
		{"设备不能收已退役的作业主题", dev, "printer/aaa/job", false, false},
		{"设备不能收已退役的档案主题", dev, "printer/aaa/profile", false, false},

		{"App 订阅名下设备", app, "printer/aaa/status", false, true},
		{"App 订阅名下另一台", app, "printer/ccc/status", false, true},
		{"App 不能订阅名下之外的", app, "printer/bbb/status", false, false},
		{"App 不能伪造状态", app, "printer/aaa/status", true, false},
		{"App 不能订阅用量", app, "printer/aaa/usage", false, false},
		{"App 不能订阅已退役的作业主题", app, "printer/aaa/job", false, false},

		// agent 只走 HTTP 上报，MQTT 上什么都不能碰——哪怕是自己名下的设备
		{"agent 不能推用量", agent, "printer/aaa/usage", true, false},
		{"agent 不能订阅状态", agent, "printer/aaa/status", false, false},
		{"agent 不能订阅用量", agent, "printer/aaa/usage", false, false},

		{"越界前缀", dev, "printer/aaa", false, false},
		{"多余层级", dev, "printer/aaa/status/x", false, false},
		{"完全无关的 topic", dev, "$SYS/broker/uptime", false, false},
	}
	for _, c := range cases {
		if got := ACLAllowed(c.id, c.topic, c.write, fakeMembership{}); got != c.want {
			t.Errorf("%s: ACLAllowed(%+v, %q, write=%v) = %v，期望 %v",
				c.name, c.id, c.topic, c.write, got, c.want)
		}
	}
}
