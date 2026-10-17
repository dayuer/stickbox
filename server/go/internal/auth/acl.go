package auth

import "strings"

// ACLAllowed 判定某身份能否读/写某 topic。
//
// topic 一律是 printer/{dev}/{leaf} 三段。T7 没有 App 角色：
// 设备写自己的 status（心跳），读自己的 usage（用量）与 pair（配对通知）；
// agent 只走 HTTP，不连 MQTT。
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
		return leaf == "status"
	}
	return leaf == "usage" || leaf == "pair"
}
