package genurf

import (
	"os"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/raster"
)

// TestEmitPage 不是测试，是拿 internal 包生成一张真页面用来端到端调试。
// 只在显式指定 -run 时有意义。
func TestEmitPage(t *testing.T) {
	out := os.Getenv("URF_OUT")
	if out == "" {
		t.Skip("未设 URF_OUT")
	}
	if err := os.WriteFile(out, raster.TestPage(2480, 3507, 300, raster.KindMark), 0o644); err != nil {
		t.Fatal(err)
	}
}
