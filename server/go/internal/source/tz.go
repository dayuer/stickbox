package source

import (
	"log/slog"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/tz"
)

// UserTZStore 是时区解析对 store 的窄依赖。
type UserTZStore interface {
	// UserTZ 返回用户级时区设置；空串表示未设置。
	UserTZ(userID string) (string, error)
}

// TZResolver 按「用户设置 > 这一路数据自带 > 服务端默认」解析时区。
//
// 时区是**用户属性**，不是某一路数据自带的属性：用户在 App / CLI 里设的值
// 必须盖过 agent 所在那台机器报上来的值（T29 的决定）。各数据源只提供
// 自己那一层的回退值——Claude 是 agent 上报的 tz，经营数据是上报方的 tz。
//
// 放在 source 而不是 frames：帧服务已经不碰取数了，时区属于「怎么解释这一路
// 数据」的一部分。优先级逻辑本身仍然是 internal/tz 那一份，没有第二实现。
type TZResolver struct {
	// Store 为 nil 时跳过用户级查询，只按「这一路自带 > 默认」解析。
	Store   UserTZStore
	Default string
}

// NewTZResolver 构造带用户级设置的解析器。
func NewTZResolver(st UserTZStore, defaultTZ string) TZResolver {
	return TZResolver{Store: st, Default: defaultTZ}
}

// NewFixedTZ 构造不查用户设置的解析器：只按「这一路自带 > 默认」。
// 单测、以及不需要用户级时区的部署用它。
func NewFixedTZ(defaultTZ string) TZResolver {
	return TZResolver{Default: defaultTZ}
}

// Location 解析该用户此刻该用的时区。routeTZ 是这一路数据自带的那个值。
//
// 读库失败不让出帧失败：退回 routeTZ / 默认，只记一条告警。
// 屏幕上时区偏一点，比整张帧出不来要好。
func (r TZResolver) Location(userID, routeTZ string) *time.Location {
	userTZ := ""
	if r.Store != nil {
		if v, err := r.Store.UserTZ(userID); err != nil {
			slog.Warn("读用户时区失败，按这一路自带/默认处理", "user", userID, "err", err)
		} else {
			userTZ = v
		}
	}
	name, _ := tz.Resolve(userTZ, routeTZ, r.Default)
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
