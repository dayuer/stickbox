package sim

import (
	"errors"
	"strings"
	"testing"
)

// 推给电视的 URL 主机必须是「连电视时本机用的局域网 IP」，
// 不能是监听地址（":8091" / "[::]:8091" 电视访问不了）。
func TestBuildStreamURLUsesDialedLANIP(t *testing.T) {
	fake := func(string) (string, error) { return "192.168.3.237", nil }

	for _, listen := range []string{":8091", "[::]:8091", "0.0.0.0:8091", "127.0.0.1:8091"} {
		u, err := BuildStreamURL(listen, "192.168.3.18", fake)
		if err != nil {
			t.Fatalf("%s: %v", listen, err)
		}
		if u != "http://192.168.3.237:8091/live.avi" {
			t.Errorf("%s -> %q", listen, u)
		}
		if strings.Contains(u, "[::]") || strings.Contains(u, "0.0.0.0") {
			t.Errorf("URL 泄漏了监听地址：%q", u)
		}
	}

	// 拿不到可用局域网 IP 时必须报错，绝不能回退到监听地址。
	for _, bad := range []string{"", "::", "0.0.0.0", "127.0.0.1", "::1", "not-an-ip"} {
		if u, err := BuildStreamURL(":8091", "tv", func(string) (string, error) { return bad, nil }); err == nil {
			t.Errorf("IP %q 应被拒，得到 %q", bad, u)
		}
	}
	if _, err := BuildStreamURL(":8091", "tv", func(string) (string, error) {
		return "", errors.New("no route")
	}); err == nil {
		t.Error("找不到路由时应报错")
	}
	if _, err := BuildStreamURL("bad", "tv", fake); err == nil {
		t.Error("非法监听地址应报错")
	}
}

func TestBuildStreamURLIPv6LAN(t *testing.T) {
	u, err := BuildStreamURL("[::]:8091", "tv", func(string) (string, error) {
		return "fd00::1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://[fd00::1]:8091/live.avi" {
		t.Errorf("IPv6 局域网地址 = %q", u)
	}
}
