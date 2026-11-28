package weather

import "strings"

// LocationNote 每轮告诉模型主人家大致在哪（实现 brain.Briefer）：
// 「附近」「东京以内」「会下雨吗」这类问题按主人的地点理解，联网搜索也用对地区与语言。
// 只给城市级（不给 IP、不给坐标）；只用已经知道的位置，不在对话里发网络请求。
type LocationNote struct {
	Home HomeLocator
}

// Brief 实现 brain.Briefer。位置里没有名字、地区、国家（例如只配了坐标）时不说。
func (n LocationNote) Brief(person string) []string {
	if n.Home == nil {
		return nil
	}
	place, byIP, ok := n.Home.Known(person)
	if !ok {
		return nil
	}
	country := place.Country
	if country == "" {
		country = place.CountryCode
	}
	var parts []string
	for _, part := range []string{place.Name, place.Region, country} {
		if part != "" {
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
