package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

const cliUsage = `用法：
  stickboxd -conf <file> healthcheck    GET /healthz（容器 healthcheck 用）
  stickboxd -conf <file> backup -out <file>
                                        在线备份 sqlite（cron 用）
  stickboxd device add <dev> [name]     手工签发 device 密钥（调试用）
  stickboxd device list                 列出全部密钥
  stickboxd device revoke <key_id>      吊销一把密钥
  stickboxd device unbind <dev>         强制解绑（原持有人不配合时用）
  stickboxd device show <dev> [--last N]
                                        设备诊断：密钥状态、最近 CONNECT、
                                        未确认 cid、用量/帧版本、注册配额
  stickboxd user list                   列出用户

正常设备走 /api/device/register 出配对码，不需要这些命令。
全局 -conf 必须放在子命令之前；默认 /opt/stickbox/config.json。`

// runCLI 是运维和排障用的。
func runCLI(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", cliUsage)
	}
	switch args[0] {
	case "healthcheck":
		return runHealthcheck(cfg)
	case "backup":
		return runBackup(cfg, args[1:])
	}
	// device show 只读运行中的 stickboxd，不打开数据库。
	if len(args) >= 2 && args[0] == "device" && args[1] == "show" {
		return runDeviceShow(cfg, args[2:])
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	v := auth.NewVerifier(st, cfg.TokenPepper)

	cmd := strings.Join(args[:min(2, len(args))], " ")
	switch cmd {
	case "device add":
		dev := arg(args, 2)
		if dev == "" {
			return fmt.Errorf("%s", cliUsage)
		}
		token, _, err := auth.IssueDeviceKey(v, st, "", dev, arg(args, 3), false)
		if err != nil {
			return err
		}
		fmt.Printf("设备密钥（只显示这一次）：\n  %s\n", token)
		return nil

	case "device list":
		keys, err := st.ListKeys()
		if err != nil {
			return err
		}
		for _, k := range keys {
			state := "启用"
			if k.Disabled {
				state = "已吊销"
			} else if k.Pending {
				state = "待配对"
			}
			fmt.Printf("%-12s %-7s dev=%-14s user=%-32s %-6s %s\n",
				k.KeyID, k.Role, k.Dev, k.UserID, state, k.Name)
		}
		return nil

	case "device revoke":
		if arg(args, 2) == "" {
			return fmt.Errorf("%s", cliUsage)
		}
		return auth.RevokeSession(st, v, arg(args, 2))

	// 抢绑防护的逃生门：原持有人不配合时用。
	// 绕过所有权检查，所以只能在服务器上执行，不暴露为 API。
	case "device unbind":
		if arg(args, 2) == "" {
			return fmt.Errorf("%s", cliUsage)
		}
		return auth.RevokeDeviceKeys(st, v, arg(args, 2))

	case "user list":
		users, err := st.ListUsers()
		if err != nil {
			return err
		}
		for _, u := range users {
			state := "启用"
			if u.Disabled {
				state = "停用"
			}
			fmt.Printf("%-32s 创建=%s  %s\n", u.ID, ts(u.Created), state)
		}
		return nil
	}
	return fmt.Errorf("未知命令 %q\n\n%s", strings.Join(args, " "), cliUsage)
}

// runHealthcheck 供容器 healthcheck 用。distroless 没有 curl/wget，
// 所以让二进制自己打一次 /healthz。
//
// 目标固定是回环地址：容器里访问自己的 HTTPS 端口，证书是公网域名的，
// 这里跳过校验是合理的（不跨网络、不传数据）。
func runHealthcheck(cfg *config.Config) error {
	host, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http_addr: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "https://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: HTTP %d", resp.StatusCode)
	}
	return nil
}

// runBackup 在线导出一份 sqlite 快照（VACUUM INTO）。
func runBackup(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("out", "", "输出数据库文件（不能已存在）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("用法: stickboxd backup -out /path/jobs.db")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := st.Backup(ctx, *out); err != nil {
		return err
	}
	fmt.Printf("已备份到 %s\n", *out)
	return nil
}

func arg(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}

func ts(v int64) string {
	if v == 0 {
		return "-"
	}
	return time.Unix(v, 0).Format("2006-01-02 15:04")
}
