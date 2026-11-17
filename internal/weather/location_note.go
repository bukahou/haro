package weather

import "strings"

// LocationNote 每轮告诉模型主人家大致在哪（实现 brain.Briefer；2026-11-17 用户同意）：
// 「附近」「东京以内」「会下雨吗」这类问题按主人的地点理解，联网搜索也用对地区与语言。
// 只给城市级（不给 IP、不给坐标）；只用已经定位好的结果，不在对话里发网络请求。
type LocationNote struct {
	Home HomeLocator
}

// cachedHome 是能立刻给出位置（不发网络请求）的 HomeLocator。
type cachedHome interface {
	Cached() (Place, bool)
}

// Brief 实现 brain.Briefer。
func (n LocationNote) Brief(string) []string {
	var place Place
	byIP := false
	switch home := n.Home.(type) {
	case FixedHome:
		place = home.Place
	case cachedHome:
		cached, ok := home.Cached()
		if !ok {
			return nil
		}
		place, byIP = cached, true
	default:
		return nil
	}
	var parts []string
	for _, part := range []string{place.Name, place.Region, place.Country} {
		if part != "" && part != "家" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	line := "主人家大约在 " + strings.Join(parts, "，")
	if byIP {
		line += "（按网络位置推定，城市级，可能不准）"
	}
	return []string{line + "。"}
}
