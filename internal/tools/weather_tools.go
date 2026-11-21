package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/weather"
)

// PlaceFinder 按地名找坐标：返回地名库里最多 limit 个候选（最相关的在前），一个都没有时返回空。
type PlaceFinder interface {
	FindPlaces(ctx context.Context, name, language string, limit int) ([]weather.Place, error)
}

// placeCandidates 是按地名找时最多要几个候选。
const placeCandidates = 5

// WeatherTools 是查天气的工具。地点不填就是家（home 给出位置；nil = 必须给地点）。说了地名就按地名库的第一个候选查，
// 同名的其他地方一并交给模型：主人说的是别处时，模型带坐标再查一次。查不到也照实交给模型，由它换个写法或问主人。
// language 是按地名找坐标时用的语言。
func WeatherTools(places PlaceFinder, forecaster weather.Forecaster, home weather.HomeLocator, language string,
	logger *slog.Logger) Builtin {
	description := "查天气：现在的天气和未来几天的预报（气温、降水概率）。主人问天气、要不要带伞、冷不冷时用；天气每时每刻都在变，不调用就不知道。"
	if home != nil {
		description += "查家里时不填地点。"
	} else {
		description += "必须给出地点（家的位置没有设置）。"
	}
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "get_weather", Description: description, Annotations: ReadOnly},
			func(ctx context.Context, in weatherInput) (weatherOutput, error) {
				var out weatherOutput
				place, found, err := pickPlace(ctx, places, home, language, in, &out)
				if err != nil || !found {
					return out, err
				}
				days := in.Days
				if days == 0 {
					days = 3
				}
				forecast, err := forecaster.ForecastFor(ctx, place, days)
				logger.Info("查天气", "place", place.Name, "days", days, "source", forecast.Source, "err", err)
				if err != nil {
					return weatherOutput{}, err
				}
				out.Forecast = &forecast
				return out, nil
			})
	}
}

// pickPlace 决定查哪里：给了坐标就用坐标；没给地名是家；给了地名查地名库。地名库里没有时 found = false，
// 说明写在 out.Note 里（不当成出错：换个写法或问主人就行）。
func pickPlace(ctx context.Context, places PlaceFinder, home weather.HomeLocator, language string, in weatherInput,
	out *weatherOutput) (place weather.Place, found bool, err error) {
	name := strings.TrimSpace(in.Place)
	switch {
	case in.Latitude != nil && in.Longitude != nil:
		return weather.Place{Name: name, Latitude: *in.Latitude, Longitude: *in.Longitude}, true, nil
	case name == "":
		if home == nil {
			return weather.Place{}, false, errors.New("家的位置没有设置（HARO_WEATHER_HOME），要说清楚查哪里")
		}
		out.Home = true
		place, err := home.Home(ctx)
		return place, err == nil, err
	}
	candidates, err := places.FindPlaces(ctx, name, language, placeCandidates)
	if err != nil {
		return weather.Place{}, false, err
	}
	if len(candidates) == 0 {
		out.Note = "地名库里没找到这个地名：可以换成正式全称（例如带上「市」「県」）或罗马字再查一次，或者问主人具体是哪里。"
		return weather.Place{}, false, nil
	}
	if len(candidates) > 1 {
		out.OtherPlaces = candidates[1:]
		out.Note = "同名的地方不止一个：先按第一个查了。主人说的是 other_places 里的另一个时，带上它的 latitude、longitude 再查一次。"
	}
	return candidates[0], true, nil
}

type weatherInput struct {
	Place     string   `json:"place,omitempty" jsonschema:"地名，例如「大阪市」「Osaka」「上海」；查家里时不填"`
	Latitude  *float64 `json:"latitude,omitempty" jsonschema:"纬度：从上一次结果的 other_places 里挑了别的地方时，和经度一起填"`
	Longitude *float64 `json:"longitude,omitempty" jsonschema:"经度：和纬度一起填"`
	Days      int      `json:"days,omitempty" jsonschema:"要几天的预报（含今天），1–7，默认 3"`
}

type weatherOutput struct {
	Forecast    *weather.Forecast `json:"forecast,omitempty"`
	Home        bool              `json:"home,omitempty"`         // 查的是家
	OtherPlaces []weather.Place   `json:"other_places,omitempty"` // 同名的其他地方
	Note        string            `json:"note,omitempty"`
}
