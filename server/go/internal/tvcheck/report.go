package tvcheck

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSON 返回报告文件内容。
func (r *Report) JSON() ([]byte, error) {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// HasErrors 表示报告里有需要处理的失败项。
func (r *Report) HasErrors() bool {
	for _, w := range r.Warnings {
		if w.Severity == "error" {
			return true
		}
	}
	return false
}

// Markdown 生成可直接贴进 GitHub issue 的摘要。
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("## StickBox 电视兼容性报告\n\n")

	tv := r.Renderer.Name
	if tv == "" {
		tv = "未知"
	}
	fmt.Fprintf(&b, "**电视**：%s\n\n", mdEscape(tv))
	if r.Renderer.Manufacturer != "" || r.Renderer.Model != "" {
		fmt.Fprintf(&b, "**厂商 / 型号**：%s / %s\n\n",
			mdEscape(orDash(r.Renderer.Manufacturer)), mdEscape(orDash(r.Renderer.Model)))
	}
	fmt.Fprintf(&b, "**控制地址**：`%s`\n\n", r.Renderer.ControlURL)
	fmt.Fprintf(&b, "**测试时间**：%s（持续 %d 秒）\n\n",
		r.GeneratedAt, r.Stream.DurationSeconds)

	b.WriteString("| 项目 | 结果 |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, mdEscape(v)) }
	row("SetAVTransportURI", statusText(r.Push.SetURIStatus))
	row("Play", statusText(r.Push.PlayStatus))
	if r.Push.TimeToPlayingMS != nil {
		row("进入 PLAYING", fmt.Sprintf("%.1fs", msToSeconds(*r.Push.TimeToPlayingMS)))
	} else {
		row("进入 PLAYING", "15 秒内未进入")
	}
	if r.Push.TransitioningMS != nil {
		row("TRANSITIONING", fmt.Sprintf("%.1fs", msToSeconds(*r.Push.TransitioningMS)))
	}
	row("拉流连接", fmt.Sprintf("%d 次 GET / HEAD，断开 %d 次", r.Stream.Connections, r.Stream.Disconnects))
	if len(r.Stream.Reconnects) > 0 {
		parts := make([]string, 0, len(r.Stream.Reconnects))
		for _, rc := range r.Stream.Reconnects {
			parts = append(parts, fmt.Sprintf("%.1fs", msToSeconds(rc.DowntimeMS)))
		}
		row("重连耗时", strings.Join(parts, "、"))
	}
	row("电视请求方法", orDash(strings.Join(r.Stream.HTTP.Methods, ", ")))
	row("User-Agent", orDash(strings.Join(r.Stream.HTTP.UserAgents, " / ")))
	row("HEAD / Range", fmt.Sprintf("HEAD=%v / Range=%v", r.Stream.HTTP.SawHead, r.Stream.HTTP.SawRange))
	if len(r.ProtocolInfo.VideoMIME) > 0 {
		row("GetProtocolInfo video", strings.Join(r.ProtocolInfo.VideoMIME, ", "))
	} else if r.ProtocolInfo.Error != "" {
		row("GetProtocolInfo", "失败："+r.ProtocolInfo.Error)
	} else {
		row("GetProtocolInfo video", "（没有声明 video/*）")
	}
	row("StickBox 判定", verdict(r))

	if len(r.Warnings) > 0 {
		b.WriteString("\n### 提示\n\n")
		for _, w := range r.Warnings {
			label := map[string]string{"info": "提示", "warn": "注意", "error": "失败"}[w.Severity]
			fmt.Fprintf(&b, "- **[%s] %s**", label, mdEscape(w.Message))
			if w.Hint != "" {
				fmt.Fprintf(&b, "：%s", mdEscape(w.Hint))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n---\n")
	b.WriteString("由 `stickbox-tvcheck` 生成。把这份摘要贴到 ")
	b.WriteString("https://github.com/dayuer/stickbox/issues ")
	b.WriteString("（兼容性报告模板），并附上 JSON 报告文件。\n")
	return b.String()
}

func verdict(r *Report) string {
	if r.Push.TimeToPlayingMS == nil {
		return "无法起播"
	}
	if r.Stream.Connections == 0 {
		return "PLAYING 但没有拉流"
	}
	if r.Stream.Disconnects == 0 {
		return "通过（长跑无断流）"
	}
	if r.Stream.FirstDisconnectAfterMS != nil && *r.Stream.FirstDisconnectAfterMS < 15000 {
		return "可用，但疑似首推断开"
	}
	return "可用，长跑有断流"
}

func statusText(st int) string {
	if st == 0 {
		return "没有返回（连接失败）"
	}
	return fmt.Sprintf("%d", st)
}

func mdEscape(s string) string {
	return strings.ReplaceAll(orDash(s), "|", "\\|")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
