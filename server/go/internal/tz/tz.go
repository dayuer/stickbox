// Package tz 负责用户时区的校验与优先级解析。
//
// 输入既可以是 IANA 名（Asia/Jakarta），也可以是 +07:00 这类固定偏移；
// 后者在 tzdata 里对应的名字是 Etc/GMT-7（IANA 的符号与 UTC 相反）。
package tz

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 时区来源。GET /api/user/tz 用它告诉调用方当前生效值是从哪来的。
const (
	SourceUser    = "user"
	SourceAgent   = "agent"
	SourceDefault = "default"
)

const maxLen = 64

var offsetRe = regexp.MustCompile(`^([+-])([0-9]{2}):([0-9]{2})$`)

// Normalize 校验并规范化一个时区名。
//
//   - IANA 名必须能被 time.LoadLocation 识别；
//   - "+07:00" / "-05:00" 这类固定偏移转成 Etc/GMT∓N（只支持整小时；
//     "+07:30" 没有对应的 Etc/GMT 名，直接拒绝）；
//   - 空串、"Local"、绝对路径和带 ".." 的名字一律拒绝。
func Normalize(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("时区不能为空")
	}
	if len(s) > maxLen {
		return "", fmt.Errorf("时区名过长（最多 %d 字节）", maxLen)
	}
	// 兼容 tzdata 的 posix/、right/ 前缀（agent 可能从 /etc/localtime 读出来）。
	s = strings.TrimPrefix(strings.TrimPrefix(s, "posix/"), "right/")

	if m := offsetRe.FindStringSubmatch(s); m != nil {
		if m[3] != "00" {
			return "", fmt.Errorf("固定偏移只支持整小时（例如 +07:00）")
		}
		sign := "+"
		if m[1] == "+" {
			sign = "-" // Etc/GMT-7 = UTC+7
		}
		// Etc/GMT 名里的小时不补零：Etc/GMT-7，而不是 Etc/GMT-07。
		hour := strings.TrimLeft(m[2], "0")
		if hour == "" {
			hour = "0"
			sign = "+" // 零偏移统一写成 Etc/GMT+0
		}
		name := "Etc/GMT" + sign + hour
		if _, err := time.LoadLocation(name); err != nil {
			return "", fmt.Errorf("固定偏移超出可表示范围：%s", s)
		}
		return name, nil
	}

	if s == "Local" || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "", fmt.Errorf("不是合法的 IANA 时区名：%q", s)
	}
	loc, err := time.LoadLocation(s)
	if err != nil || loc == nil {
		return "", fmt.Errorf("无法识别的时区：%q", s)
	}
	return s, nil
}

// Resolve 按「用户设置 > agent 上报 > 服务端默认」选出生效时区。
// 任何一层非法都跳过，最终兜底 UTC。返回值里同时带来源。
func Resolve(userTZ, agentTZ, defaultTZ string) (string, string) {
	if z, err := Normalize(userTZ); err == nil {
		return z, SourceUser
	}
	if z, err := Normalize(agentTZ); err == nil {
		return z, SourceAgent
	}
	if z, err := Normalize(defaultTZ); err == nil {
		return z, SourceDefault
	}
	return "UTC", SourceDefault
}
