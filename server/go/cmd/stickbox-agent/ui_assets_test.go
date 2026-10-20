package main

import (
	"regexp"
	"strings"
	"testing"
)

// 页面不得引用任何外部资源：无 CDN、无外链字体、无内联脚本/样式（CSP 也不允许）。
func TestUIAssetsSelfContained(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"index.html", "app.js", "app.css"} {
		b, err := uiAssets.ReadFile("ui_assets/" + n)
		if err != nil || len(b) == 0 {
			t.Fatalf("缺少内嵌资源 %s：%v", n, err)
		}
		files[n] = string(b)
	}
	for n, s := range files {
		for _, bad := range []string{"http://", "https://", "//cdn", "@import", "url(", "fonts.googleapis", "<iframe", "eval(", "document.write", "innerHTML"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s 含有不允许的内容 %q", n, bad)
			}
		}
	}
	html := files["index.html"]
	// 只允许同源的 /app.css 与 /app.js
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if m[1] != "/app.js" && m[1] != "/app.css" {
			t.Errorf("index.html 引用了 %q", m[1])
		}
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>`).FindAllString(html, -1) == nil {
		t.Error("应有 script 标签")
	}
	for _, m := range regexp.MustCompile(`<script([^>]*)>`).FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], `src="/app.js"`) {
			t.Errorf("不允许内联脚本：%q", m[0])
		}
	}
	if strings.Contains(html, "<style") || regexp.MustCompile(`\sstyle=`).MatchString(html) || regexp.MustCompile(`\son[a-z]+=`).MatchString(html) {
		t.Error("不允许内联样式或事件属性")
	}
}

// 页面里必须有失败提示的关键文案（码过期要提示电视上的码每 10 分钟换新）。
func TestUIAssetsMentionCodeRotation(t *testing.T) {
	b, _ := uiAssets.ReadFile("ui_assets/index.html")
	if !strings.Contains(string(b), "每 10 分钟换新") || !strings.Contains(string(b), "最新的码") {
		t.Error("页面应提示电视上的码每 10 分钟换新")
	}
}
