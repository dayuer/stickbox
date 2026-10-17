package auth

import "strings"

// Membership 回答「这个 dev 属于这个 user 吗」。
// 实现方负责缓存——ACL 判定在 MQTT 热路径上，不能每次查库。
type Membership interface {
	HasDevice(userID, dev string) bool
}

// ACLAllowed 判定某身份能否读/写某 topic。
//
// topic 一律是 printer/{dev}/{leaf} 三段。不做通配符展开——
// 设备和 App 都不需要跨设备订阅，把口子焊死比事后审计便宜。
//
// 打印退役后设备只剩两个通道：写 status（心跳，判在线）、读 usage（用量）。
// job / profile / ident / cmd 这些打印链路的主题已经删掉。
func ACLAllowed(id Identity, topic string, write bool, m Membership) bool {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "printer" {
		return false
	}
	dev, leaf := parts[1], parts[2]

	switch id.Role {
	case RoleDevice:
		if dev != id.Dev {
			return false
		}
		if write {
			return leaf == "status"
		}
		return leaf == "usage"

	case RoleApp:
		// App 只旁听自己名下设备的状态。
		// 能写 status 就能伪造设备在线；usage 是设备侧的数据，
		// App 走 HTTP 读即可，不需要也不允许直接订阅。
		if write || leaf != "status" {
			return false
		}
		return m != nil && m.HasDevice(id.UserID, dev)
	}
	return false
}
