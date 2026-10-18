package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/admin"
	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/usage"
)

// runDeviceShow 查询本机 stickboxd 的只读诊断接口。它不在服务器上时
// 无法得到运行期信息（最近 CONNECT、未确认 cid、限流余量）。
func runDeviceShow(cfg *config.Config, args []string) error {
	dev, last, err := parseDeviceShowArgs(args)
	if err != nil {
		return err
	}
	rep, err := fetchDeviceReport(cfg, dev, last)
	if err != nil {
		return err
	}
	fmt.Print(formatDeviceShow(rep))
	return nil
}

func parseDeviceShowArgs(args []string) (dev string, last int, err error) {
	last = 10
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--last":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--last 需要一个数字")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return "", 0, fmt.Errorf("--last 必须是正整数：%q", args[i])
			}
			last = n
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", 0, fmt.Errorf("未知参数：%s", args[i])
			}
			if dev != "" {
				return "", 0, fmt.Errorf("只能指定一个 dev")
			}
			dev = args[i]
		}
	}
	if !auth.ValidDev(dev) {
		return "", 0, fmt.Errorf("dev 必须是 12 位小写十六进制，例如 f412fa87c9e0")
	}
	if last > 50 {
		last = 50
	}
	return dev, last, nil
}

func fetchDeviceReport(cfg *config.Config, dev string, last int) (admin.Report, error) {
	if strings.TrimSpace(cfg.AdminAddr) == "" {
		return admin.Report{}, fmt.Errorf("配置里没有 admin_addr，无法查询运行期诊断")
	}
	u := "http://" + cfg.AdminAddr + "/device?dev=" + url.QueryEscape(dev) +
		"&n=" + strconv.Itoa(last)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return admin.Report{}, fmt.Errorf("连不上本机诊断接口 %s：%w（stickboxd 是否在运行？）", cfg.AdminAddr, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return admin.Report{}, fmt.Errorf("诊断接口返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var rep admin.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return admin.Report{}, fmt.Errorf("诊断接口返回了无法解析的 JSON：%w", err)
	}
	return rep, nil
}

func formatDeviceShow(r admin.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "设备 %s\n", r.Dev)
	if r.Bound {
		fmt.Fprintf(&b, "绑定：用户 %s\n", r.UserID)
	} else {
		b.WriteString("绑定：未绑定\n")
	}

	b.WriteString("密钥：\n")
	if len(r.Keys) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, k := range r.Keys {
		fmt.Fprintf(&b, "  %s  %-8s role=%s", k.KeyID, k.State, k.Role)
		if k.UserID != "" {
			fmt.Fprintf(&b, " user=%s", k.UserID)
		}
		if k.Created > 0 {
			fmt.Fprintf(&b, " 创建=%s", fmtTime(k.Created))
		}
		if k.LastSeen > 0 {
			fmt.Fprintf(&b, " 最近使用=%s", fmtTime(k.LastSeen))
		}
		b.WriteString("\n")
	}

	if len(r.ConnectedKeys) == 0 {
		b.WriteString("当前连接：无\n")
	} else {
		fmt.Fprintf(&b, "当前连接：%s\n", strings.Join(r.ConnectedKeys, ", "))
	}

	fmt.Fprintf(&b, "最近 CONNECT（%d 次）：\n", len(r.RecentAuth))
	if len(r.RecentAuth) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, e := range r.RecentAuth {
		fmt.Fprintf(&b, "  %s  0x%02x  %-16s %s\n",
			e.At.Local().Format("01-02 15:04:05"), e.Code, e.Result, e.Remote)
	}

	fmt.Fprintf(&b, "未确认指令：%d 条\n", len(r.Pending))
	for _, p := range r.Pending {
		fmt.Fprintf(&b, "  cid=%s topic=%s tries=%d next=%s\n",
			p.CID, p.Topic, p.Tries, p.NextAt.Local().Format("01-02 15:04:05"))
	}

	if !r.Bound {
		b.WriteString("用量：—（未绑定）\n")
	} else if r.Usage == nil {
		b.WriteString("用量：还没有上报\n")
	} else {
		fmt.Fprintf(&b, "用量：五小时 %s / 七天 %s（收到 %s",
			windowText(r.Usage.FiveHour), windowText(r.Usage.SevenDay),
			r.Usage.ReceivedAt.Local().Format("01-02 15:04"))
		if r.Usage.TZ != "" {
			fmt.Fprintf(&b, "，时区 %s", r.Usage.TZ)
		}
		b.WriteString("）\n")
	}

	switch {
	case !r.Bound:
		b.WriteString("帧版本：—（未绑定）\n")
	case r.Frame == nil:
		b.WriteString("帧版本：无（帧服务不可用）\n")
	case !r.Frame.Available:
		b.WriteString("帧版本：尚未生成\n")
	default:
		fmt.Fprintf(&b, "帧版本：v%d\n", r.Frame.Version)
	}

	if r.Photo == nil {
		if r.Bound {
			b.WriteString("照片：设备未上报（老固件或无 SD 构建）\n")
		}
	} else {
		fmt.Fprintf(&b, "照片：cap=%v ready=%v state=%s plv=%d 已缓存=%d 剩余=%.1fGB",
			r.Photo.Cap, r.Photo.Ready, r.Photo.State, r.Photo.PLV,
			r.Photo.Have, float64(r.Photo.FreeKB)/1024/1024)
		if r.Photo.InitDone != "" {
			fmt.Fprintf(&b, "（已用初始化 token %s）", r.Photo.InitDone)
		}
		b.WriteString("\n")
		if len(r.Photo.Bad) > 0 {
			fmt.Fprintf(&b, "  跳过：%s\n", strings.Join(r.Photo.Bad, ", "))
		}
	}

	fmt.Fprintf(&b, "注册配额：剩余 %d/%d", r.RegisterQuota.Remaining, r.RegisterQuota.Limit)
	if r.RegisterQuota.ResetAt != nil {
		fmt.Fprintf(&b, "（%s 恢复一个名额）", r.RegisterQuota.ResetAt.Local().Format("15:04:05"))
	}
	b.WriteString("\n")
	return b.String()
}

func fmtTime(ts int64) string {
	return time.Unix(ts, 0).Local().Format("01-02 15:04")
}

func windowText(w *usage.Window) string {
	if w == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", w.UsedPercentage)
}
