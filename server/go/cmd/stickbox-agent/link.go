package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// 手机扫码网页配对（W7）：网页给出一次性链接，`pair --link` 去服务端兑换 agent 令牌。
//
// 链接等同一次性凭据：不回显、不进日志、不进错误文本。所有报错只说明哪一类问题。

var (
	errBadLinkShape = errors.New("--link 不是有效的配对链接（应形如 https://站点/r/…）")
	errLinkScheme   = errors.New("--link 只接受 https:// 链接（本机联调可用 http://127.0.0.1）")
)

// parseLinkOrigin 校验链接形状并返回服务端根（scheme://host）。
func parseLinkOrigin(link string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" || !strings.HasPrefix(u.Path, "/r/") || len(u.Path) <= len("/r/") {
		return "", errBadLinkShape
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		// 链接本身是一次性凭据，明文 HTTP 只允许本机回环。
		if !isLoopbackHost(u.Hostname()) {
			return "", errLinkScheme
		}
	default:
		return "", errLinkScheme
	}
	if u.User != nil {
		return "", errBadLinkShape
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

// redeemAndSave：兑换链接并保存令牌。c.Server 须已设成链接所属的服务端。
func redeemAndSave(c *agentConfig, link, agentName string) (dev string, err error) {
	if c.Token != "" {
		return "", errors.New("本机已配对过账号；链接会创建新账号并覆盖现有令牌，" +
			"请先运行 stickbox-agent logout，或改用 stickbox-agent pair <配对码> 把新屏并入现有账号")
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return "", err
	}
	var out struct {
		Dev   string `json:"dev"`
		Token string `json:"token"`
	}
	if err := cl.call("POST", "/api/agent/redeem", false,
		map[string]string{"link": link, "name": agentName}, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", postPairError{errors.New("服务端没有返回 agent 令牌")}
	}
	c.Token, c.Name, c.PairedAt = out.Token, agentName, time.Now().Unix()
	if err := saveConfig(c); err != nil {
		return "", postPairError{err}
	}
	return out.Dev, nil
}

func pairWithLink(link, server, name string, insecure bool, out io.Writer) error {
	origin, err := parseLinkOrigin(link)
	if err != nil {
		return err
	}
	c, err := loadPairConfig("", insecure)
	if err != nil {
		return err
	}
	if server != "" {
		want, err := normalizeServer(server)
		if err != nil {
			return err
		}
		if !strings.EqualFold(want, origin) {
			return errors.New("--server 与链接所属的服务端不一致")
		}
	}
	c.Server = origin
	dev, err := redeemAndSave(c, strings.TrimSpace(link), resolveAgentName(name))
	if err != nil {
		var pp postPairError
		if errors.As(err, &pp) {
			return err
		}
		return fmt.Errorf("配对失败：%w", err)
	}
	fmt.Fprintf(out, "配对成功：%s（新用户）\n", dev)
	fmt.Fprintln(out, "接下来：stickbox-agent run --once 试跑一轮，或 stickbox-agent install 开机常驻")
	return nil
}
