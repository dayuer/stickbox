package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

const cliUsage = `用法：
  stickboxd device add <dev> [name]     手工签发 device 密钥（调试用）
  stickboxd device list                 列出全部密钥
  stickboxd device revoke <key_id>      吊销一把密钥
  stickboxd device unbind <dev>         强制解绑（原持有人不配合时用）
  stickboxd user list                   列出用户

正常设备走 /api/device/register 出配对码，不需要这些命令。`

// runCLI 是运维和排障用的。
func runCLI(cfg *config.Config, args []string) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	v := auth.NewVerifier(st)

	cmd := strings.Join(args[:min(2, len(args))], " ")
	switch cmd {
	case "device add":
		dev := arg(args, 2)
		if dev == "" {
			return fmt.Errorf("%s", cliUsage)
		}
		token, _, err := auth.IssueDeviceKey(st, "", dev, arg(args, 3), false)
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
