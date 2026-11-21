package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/weather"
)

// fakeOpenMeteo 回固定的地名与预报，记下查的坐标。地名库：「大阪市」一个，「府中」两个同名的，别的都没有
// （真实服务不认简称「大阪」）。
func fakeOpenMeteo(t *testing.T, queried *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			switch r.URL.Query().Get("name") {
			case "大阪市":
				_, _ = w.Write([]byte(`{"results":[{"name":"大阪市","latitude":34.69,"longitude":135.5,"country":"日本","admin1":"大阪府"}]}`))
			case "府中":
				_, _ = w.Write([]byte(`{"results":[{"name":"府中市","latitude":35.67,"longitude":139.48,"country":"日本","admin1":"東京都"},` +
					`{"name":"府中市","latitude":34.57,"longitude":133.24,"country":"日本","admin1":"広島県"}]}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		case "/ip":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.7","city":"Exampleville","region":"Example","country":"ZZ","loc":"12.3456,65.4321"}`))
		case "/forecast":
			*queried = r.URL.Query().Get("latitude") + "," + r.URL.Query().Get("longitude") + "," + r.URL.Query().Get("forecast_days")
			_, _ = w.Write([]byte(`{"current":{"time":"2026-11-17T10:00","temperature_2m":12.3,"apparent_temperature":10.1,
				"relative_humidity_2m":60,"weather_code":61,"wind_speed_10m":8.5},
				"daily":{"time":["2026-11-17","2026-11-18"],"weather_code":[61,0],"temperature_2m_max":[15,18],
				"temperature_2m_min":[8,9],"precipitation_probability_max":[80,10],"precipitation_sum":[5.2,0]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func callWeather(t *testing.T, box *Toolbox, arguments map[string]any) (weatherOutput, error) {
	t.Helper()
	result, err := box.Call(context.Background(), brain.ToolCall{Name: "get_weather", Arguments: arguments})
	if err != nil {
		return weatherOutput{}, err
	}
	var out weatherOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("结果不是 JSON：%s", result)
	}
	return out, nil
}

// 不填地点是家；填了地名查地名库：一个就查它；同名的几个先查第一个、其余交给模型（带坐标再查）；
// 地名库里没有就照实交给模型（不当出错，也不替它补字再查）。「家」只是普通的地名，代码不认这个词。
func TestWeatherTool(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var queried string
	server := fakeOpenMeteo(t, &queried)
	defer server.Close()
	client := weather.NewClient(server.URL+"/forecast", server.URL+"/search", server.Client())
	home := weather.FixedHome{Place: weather.Place{Latitude: 10.5, Longitude: 20.5}}
	box, err := NewToolbox(ctx, nil, logger, WeatherTools(client, client, home, "ja", logger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	out, err := callWeather(t, box, nil)
	if err != nil || !out.Home || out.Forecast == nil || out.Forecast.Current.Weather != "小雨" || queried != "10.5000,20.5000,3" {
		t.Fatalf("不说地点查家里、默认 3 天：%+v %v %s", out, err, queried)
	}
	out, err = callWeather(t, box, map[string]any{"place": "大阪市", "days": 2})
	if err != nil || out.Home || out.Forecast == nil || out.Forecast.Place.Name != "大阪市" || len(out.OtherPlaces) != 0 ||
		queried != "34.6900,135.5000,2" {
		t.Errorf("地名库里只有一个：直接查它：%+v %v %s", out, err, queried)
	}
	out, err = callWeather(t, box, map[string]any{"place": "府中"})
	if err != nil || out.Forecast == nil || out.Forecast.Place.Region != "東京都" || len(out.OtherPlaces) != 1 ||
		out.OtherPlaces[0].Region != "広島県" || out.Note == "" {
		t.Errorf("同名的两个：先查第一个，另一个交给模型：%+v %v", out, err)
	}
	other := out.OtherPlaces[0]
	out, err = callWeather(t, box, map[string]any{"place": "府中市", "latitude": other.Latitude, "longitude": other.Longitude})
	if err != nil || out.Forecast == nil || queried != "34.5700,133.2400,3" {
		t.Errorf("模型挑了另一个、带坐标再查：%+v %v %s", out, err, queried)
	}
	for _, name := range []string{"大阪", "家"} {
		queried = ""
		out, err = callWeather(t, box, map[string]any{"place": name})
		if err != nil || out.Forecast != nil || out.Home || out.Note == "" || queried != "" {
			t.Errorf("地名库里没有「%s」：照实交给模型（不查家、不补字再查、不报错）：%+v %v %s", name, out, err, queried)
		}
	}

	noHome, err := NewToolbox(ctx, nil, logger, WeatherTools(client, client, nil, "ja", logger))
	if err != nil {
		t.Fatal(err)
	}
	defer noHome.Close()
	if _, err := callWeather(t, noHome, nil); err == nil || !strings.Contains(err.Error(), "HARO_WEATHER_HOME") {
		t.Errorf("没设家的位置又没说地点：%v", err)
	}
}

func TestIPHome(t *testing.T) {
	var queried string
	server := fakeOpenMeteo(t, &queried)
	defer server.Close()
	home := weather.NewIPHome(server.URL+"/ip", "", server.Client())
	place, err := home.Home(context.Background())
	if err != nil || place.Name != "Exampleville" || place.Latitude != 12.3456 || place.Longitude != 65.4321 {
		t.Fatalf("按 IP 定位：%+v %v", place, err)
	}
	server.Close() // 定位服务连不上：沿用缓存
	if again, err := home.Home(context.Background()); err != nil || again != place {
		t.Errorf("缓存：%+v %v", again, err)
	}
	if _, err := weather.NewIPHome(server.URL+"/ip", "家", server.Client()).Home(context.Background()); err == nil {
		t.Error("第一次就连不上应报错")
	}
}
