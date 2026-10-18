package main

import (
	"encoding/json"
	"errors"
)

// totalCostMarker 是 claude 未登录时 `-p /usage` 的输出特征：
// 退化成按量计费摘要「Total cost: $0.0000 …」，而不是订阅用量。
const totalCostMarker = "Total cost:"

// errClaudeNotLoggedIn 是 `claude auth status` 确认未登录后的明确提示。
var errClaudeNotLoggedIn = errors.New("claude 未登录，请在终端执行 `claude auth login`")

// noReportError：输出里没有 usage_report。TotalCost 表示输出里出现了
// 「Total cost:」，高度怀疑是未登录，需要再用 auth status 核实。
type noReportError struct{ TotalCost bool }

func (e *noReportError) Error() string {
	return "claude 输出里没有 usage_report（/usage 的输出格式可能变了）"
}

// authState 是 `claude auth status` 的判定结果。
type authState int

const (
	authUnknown   authState = iota // 没查或查不出来
	authLoggedIn                   // loggedIn:true
	authLoggedOut                  // loggedIn:false
)

func (a authState) String() string {
	switch a {
	case authLoggedIn:
		return "已登录"
	case authLoggedOut:
		return "未登录"
	}
	return "未知"
}

// parseAuthStatus 解析 `claude auth status` 的 JSON。
func parseAuthStatus(out []byte) authState {
	var v struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out, &v) != nil || v.LoggedIn == nil {
		return authUnknown
	}
	if *v.LoggedIn {
		return authLoggedIn
	}
	return authLoggedOut
}

// claudeAuthStatus 沿用 /usage 同样的白名单环境和专用空目录。
// 未登录时 claude 可能以非 0 退出，所以出错时也看 stdout。
func claudeAuthStatus(claude, work string) authState {
	out, _ := runClaude(claude, work, "auth", "status")
	return parseAuthStatus(out)
}
