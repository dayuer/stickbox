package profile

import (
	"errors"
	"fmt"
)

// 用户能给的判断（API 文档 4.10）。
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictUnclear = "unclear"
	VerdictAborted = "aborted"
)

// ValidVerdict 判断结论是不是合法取值。
func ValidVerdict(v string) bool {
	switch v {
	case VerdictPass, VerdictFail, VerdictUnclear, VerdictAborted:
		return true
	}
	return false
}

// Outcome 是一次测试判定的结果。
type Outcome struct {
	// Profile 是应用判定之后的新档案。Applied 为空时它等于传入的 base。
	Profile Profile
	// Applied 是这次改了哪些字段，原样回给 App 展示（文档 4.10 的 applied）。
	Applied map[string]any
	// MarginsMM 非 nil 时表示页边距要改成这个值。
	MarginsMM []int
	// State 是这个测试之后的显示状态（文档 4.8 的 state）。
	State string
}

// StateFor 把结论映射成 4.8 里的 state。
func StateFor(verdict string) string {
	switch verdict {
	case VerdictPass:
		return "passed"
	case VerdictFail:
		return "failed"
	case VerdictAborted:
		return "untested" // 中止等于没测过，别在界面上留一个说不清的状态
	default:
		return "unclear"
	}
}

// Apply 把用户的判断翻译成档案改动。
//
// 所有测试的变体都是「去掉那个可能不必要的动作」，所以语义是统一的：
//
//	pass    → 去掉它也行，那就真去掉
//	fail    → 去掉不行，说明必需，恢复保守值
//	unclear → **什么都不改**
//
// unclear 不改是关键。文档 4.10 说「用户不确定时必须让他选 unclear，
// 不要逼他二选一——猜出来的数据比没有数据更糟」。那句话在这里的落点就是
// 这个分支：宁可这台机器一直用保守配置，也不要把一个猜测写进众包库，
// 再经由 model 层扩散到同型号所有人身上。
func Apply(base Profile, test, verdict string, detail map[string]any) (Outcome, error) {
	if !ValidVerdict(verdict) {
		return Outcome{}, fmt.Errorf("profile: 非法的 verdict %q", verdict)
	}
	out := Outcome{Profile: base, Applied: map[string]any{}, State: StateFor(verdict)}

	if verdict == VerdictUnclear || verdict == VerdictAborted {
		return out, nil
	}

	switch test {
	case TestDoublePrint:
		// 变体去掉了作业结束符。
		// pass（两份都出）= 不需要它；fail（只出一份）= 需要它。
		if verdict == VerdictPass {
			out.Profile.Hooks.JobEnd = []Step{}
			out.Applied["uel_job_end"] = false
		} else {
			out.Profile.Hooks.JobEnd = []Step{{Op: OpSendHex, Data: UEL, Required: true}}
			out.Applied["uel_job_end"] = true
		}

	case TestWake:
		if verdict == VerdictPass {
			out.Profile.Hooks.Wake = []Step{}
			out.Applied["uel_wake"] = false
		} else {
			steps := []Step{{Op: OpSendHex, Data: UEL, Required: true}}
			// detail.delay_s 是用户量到的唤醒耗时。它决定唤醒后要等多久再送数据，
			// 太短的话打印机还没准备好，前几 KB 会被丢掉。
			if ms := delayMS(detail); ms > 0 {
				steps = append(steps, Step{Op: OpDelayMS, MS: ms})
				out.Applied["wake_delay_ms"] = ms
			}
			out.Profile.Hooks.Wake = steps
			out.Applied["uel_wake"] = true
		}

	case TestMargins:
		cut, err := cutMM(detail)
		if err != nil {
			return Outcome{}, err
		}
		// 被切掉多少就往里收多少。方向只能是变大——把页边距改小会让下一次
		// 切得更多，而用户已经看到过一次被切的纸了。
		out.MarginsMM = cut
		out.Applied["margins_mm"] = cut

	default:
		return Outcome{}, fmt.Errorf("profile: 不认识的测试 %q", test)
	}

	return out, nil
}

// delayMS 从 detail.delay_s 折算成毫秒，并夹到 profile 的硬上限内。
func delayMS(detail map[string]any) int {
	v, ok := detail["delay_s"]
	if !ok {
		return 0
	}
	f, ok := v.(float64) // JSON 数字
	if !ok || f <= 0 {
		return 0
	}
	ms := int(f * 1000)
	if ms > MaxDelayMS {
		ms = MaxDelayMS
	}
	return ms
}

// cutMM 读 detail.cut_mm：[上, 右, 下, 左]，单位毫米。
func cutMM(detail map[string]any) ([]int, error) {
	raw, ok := detail["cut_mm"].([]any)
	if !ok || len(raw) != 4 {
		return nil, errors.New("profile: margins 需要 detail.cut_mm = [上,右,下,左]")
	}
	out := make([]int, 4)
	for i, v := range raw {
		f, ok := v.(float64)
		if !ok || f < 0 || f > 50 {
			return nil, errors.New("profile: cut_mm 的每个值要在 0~50 之间")
		}
		out[i] = int(f)
	}
	return out, nil
}
