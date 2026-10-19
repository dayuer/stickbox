package main

import (
	"context"
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

// retryPlanner 决定一轮结束后睡多久。只在 runDaemon 的单个循环里使用，
// 不需要加锁。
type retryPlanner struct {
	fast int // 当前这串 transient 已经用掉的快速重试次数
	fail int // 连续失败轮数（未配对不算故障，既不累计也不清零，与 failureTracker 一致）
}

// next 根据本轮结果返回下一轮前的等待时间。
//   - 成功：计数清零，usageInterval（成功路径不变）。
//   - transient：前 3 次依次 60s/120s/240s；用完之后回 usageInterval，
//     且不再开新一串快速重试，直到出现一次成功或 persistent，避免长时间故障期间
//     每 10 分钟都夹带一串重试。
//   - persistent：保持原有间隔（未配对/令牌失效 1 小时，其余 usageInterval），
//     不快速重试，也就不会反复拉起 claude。
func (p *retryPlanner) next(err error) (wait time.Duration, fastRetry bool) {
	switch classify(err) {
	case outcomeOK:
		p.fast, p.fail = 0, 0
		return usageInterval, false
	case outcomeTransient:
		p.fail++
		if p.fast < len(fastRetryDelays) {
			d := fastRetryDelays[p.fast]
			p.fast++
			return d, true
		}
		return usageInterval, false
	}
	if !errors.Is(err, errNotPaired) {
		p.fail++
	}
	p.fast = 0
	return retryDelay(err), false
}

// daemonConfig 是 runDaemon 的依赖。sleep / now 注入，单测里不真的等。
type daemonConfig struct {
	logf  func(string, ...any)
	round func() error
	sleep func(context.Context, time.Duration) bool
	now   func() time.Time
	// onSchedule 在每轮结束、睡眠之前调用：连续失败轮数（成功为 0）和下次尝试时间。
	// 可为 nil。
	onSchedule func(fails int, next time.Time)
}

// runDaemon 常驻循环：一轮跑完才睡、睡醒才跑下一轮，所以任何时候最多
// 一个 claude 子进程，重试不会重叠。sleep 注入是为了单测里不真的等 1 小时。
func runDaemon(
	ctx context.Context,
	logf func(string, ...any),
	round func() error,
	sleep func(context.Context, time.Duration) bool,
) error {
	return runDaemonWith(ctx, daemonConfig{logf: logf, round: round, sleep: sleep, now: time.Now})
}

func runDaemonWith(ctx context.Context, c daemonConfig) error {
	var plan retryPlanner
	for {
		err := c.round()
		wait, fast := plan.next(err)
		if err != nil {
			if fast {
				c.logf("本轮失败（瞬时，第 %d/%d 次快速重试）：%v（%s 后重试）",
					plan.fast, len(fastRetryDelays), err, wait)
			} else {
				c.logf("本轮失败：%v（%s 后重试）", err, wait)
			}
		}
		if c.onSchedule != nil {
			c.onSchedule(plan.fail, c.now().Add(wait))
		}
		if !c.sleep(ctx, wait) {
			c.logf("收到退出信号，停止")
			return nil
		}
	}
}
