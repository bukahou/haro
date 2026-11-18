package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/weather"
)

// PlaceFinder 按地名找坐标。
type PlaceFinder interface {
	FindPlace(ctx context.Context, name, language string) (weather.Place, error)
}

// WeatherTools 返回查天气的内置工具。places 按地名找坐标；forecaster 查天气（可以带备用来源）；
// home 给出「家」的位置（配置的坐标，或按 IP 定位），nil = 必须说地名。
func WeatherTools(places PlaceFinder, forecaster weather.Forecaster, home weather.HomeLocator, logger *slog.Logger) []Builtin {
	description := "查天气：现在的天气和未来几天的预报（气温、降水概率）。主人问天气、要不要带伞、冷不冷时用；天气每时每刻都在变，不调用就不知道。"
	if home != nil {
		description += "不说地点就是查家里。"
	} else {
		description += "必须给出地点（家的位置没有设置）。"
	}
	return []Builtin{{
		Spec: brain.ToolSpec{Name: "get_weather", Description: description,
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"place":{"type":"string","description":"地名，用正式全称或罗马字，例如「大阪市」「Osaka」「上海」；查家里时不填"},` +
				`"days":{"type":"integer","description":"要几天的预报（含今天），1–7，默认 3"}}}`)},
		Run: func(ctx context.Context, arguments map[string]any) (string, error) {
			var in struct {
				Place string `json:"place"`
				Days  int    `json:"days"`
			}
			if err := decodeArguments(arguments, &in); err != nil {
				return "", err
			}
			if in.Days == 0 {
				in.Days = 3
			}
			place, err := resolvePlace(ctx, places, home, strings.TrimSpace(in.Place))
			if err != nil {
				return "", err
			}
			forecast, err := forecaster.ForecastFor(ctx, place, in.Days)
			logger.Info("查天气", "place", place.Name, "days", in.Days, "source", forecast.Source, "err", err)
			if err != nil {
				return "", err
			}
			return marshalResult(forecast)
		},
	}}
}

// resolvePlace：没说地点用家；说了就查坐标（返回的地名用日语写法）。
func resolvePlace(ctx context.Context, places PlaceFinder, home weather.HomeLocator, name string) (weather.Place, error) {
	if name == "" || name == "家" || name == "家里" {
		if home == nil {
			return weather.Place{}, errors.New("家的位置没有设置（HARO_WEATHER_HOME），要说清楚查哪里")
		}
		return home.Home(ctx)
	}
	place, err := places.FindPlace(ctx, name, "ja")
	// 实测（2026-11-17）：Open-Meteo 的地名库认「大阪市」「Osaka」，不认简称「大阪」⇒ 找不到时补个「市」再找一次
	if errors.Is(err, weather.ErrPlaceNotFound) && !strings.HasSuffix(name, "市") && !isASCII(name) {
		place, err = places.FindPlace(ctx, name+"市", "ja")
	}
	return place, err
}

func isASCII(text string) bool {
	for _, r := range text {
		if r > 127 {
			return false
		}
	}
	return true
}
