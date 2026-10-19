package main

import (
	"crypto/x509"
	"errors"
	"net/http"
	"time"
)

// outcome 是一轮上报的结果分类，决定下一轮多久后开始。
type outcome int

const (
	outcomeOK         outcome = iota // 成功：正常间隔
	outcomeTransient                 // 瞬时：网络抖动、5xx、限额暂不可用，快速重试
	outcomePersistent                // 持续：要人处理，别反复拉起 claude
)

func (o outcome) String() string {
	switch o {
	case outcomeOK:
		return "ok"
	case outcomeTransient:
		return "transient"
	}
	return "persistent"
}

// 瞬时失败的快速重试节奏：60s → 120s → 240s，共 3 次，之后回到 usageInterval。
// 每次重试都是完整的一轮（拉起 claude + 上报），由 runDaemon 串行驱动，
// 不会重叠；claude 单次峰值 350–400MB，所以不能更密也不能并发。
var fastRetryDelays = []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}

// skipError：本轮没有可报的数据（rate_limits 为 null，多半是 429 冷却或
// claude 刚启动还没取到），不是配置问题，归为瞬时。
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

// transportError：请求没有拿到 HTTP 响应（超时、连接被拒、DNS 失败等）。
// 消息文本与加类型之前完全一致。
type transportError struct {
	msg string
	err error
}

func (e *transportError) Error() string { return e.msg }
func (e *transportError) Unwrap() error { return e.err }

// classify 把一轮的 error 归类。拿不准的一律算 persistent：
// 宁可保持 10 分钟间隔，也不要反复拉起 claude。
func classify(err error) outcome {
	if err == nil {
		return outcomeOK
	}
	var se *skipError
	if errors.As(err, &se) {
		return outcomeTransient
	}
	var ae *apiError
	if errors.As(err, &ae) {
		// 401/403 令牌失效、4xx 请求有问题：重试没用。只有 5xx 是服务端的瞬时故障。
		if ae.Status >= http.StatusInternalServerError {
			return outcomeTransient
		}
		return outcomePersistent
	}
	var te *transportError
	if errors.As(err, &te) {
		// 证书问题不会自己好。
		var ua x509.UnknownAuthorityError
		var ci x509.CertificateInvalidError
		var hn x509.HostnameError
		if errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &hn) {
			return outcomePersistent
		}
		return outcomeTransient
	}
	return outcomePersistent
}

// isForbidden 与 isUnauthorized 一起，表示令牌被服务端拒绝。
func isForbidden(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden
}
