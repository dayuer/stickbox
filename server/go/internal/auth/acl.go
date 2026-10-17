package auth

import "strings"

// ACLAllowed 判定某身份能否读/写某 topic。
//
// topic 一律是 printer/{dev}/{leaf} 三段。T7 没有 App 角色：
// 设备写自己的 status（心跳）与 ack（下行指令回执），读自己的 usage（用量）、
// pair（配对通知）与 frame（帧版本通知）；agent 只走 HTTP，不连 MQTT。
//
// 关键：pending 密钥只代表「知道这个 MAC」，不代表设备的合法主人。
// 它只能读 pair（等配对）、写 status（心跳）。
//
// **pending 尤其不能写 ack**：用受害者 MAC 注册出来的 pending 密钥，dev 和
// 受害设备完全一样，而 command.Tracker 只比对 dev。放它写 ack，攻击者就能
// 替受害设备伪造回执，把人家的 unpair 或 usage 提前结案——受害设备永远收不到
// 那条指令，服务端却以为成功了。
func ACLAllowed(id Identity, topic string, write bool) bool {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "printer" {
		return false
	}
	dev, leaf := parts[1], parts[2]

	if id.Role != RoleDevice || dev != id.Dev {
		return false
	}
	if write {
		if id.Pending {
			return leaf == "status"
		}
		// ack：设备确认已**应用**某条下行指令。QoS1 只保证投递到设备的
		// MQTT 栈，不保证设备应用成功——没有这条回执，两端状态会悄悄分叉。
		return leaf == "status" || leaf == "ack"
	}
	if id.Pending {
		return leaf == "pair"
	}
	return leaf == "usage" || leaf == "pair" || leaf == "frame"
}
