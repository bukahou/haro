package tools

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/weather"
)

// fakeOpenMeteo 回固定的地名与预报，记下查的坐标。
func fakeOpenMeteo(t *testing.T, queried *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if name := r.URL.Query().Get("name"); name == "大阪" || strings.HasPrefix(name, "不存在的地方") { // 真实服务不认简称「大阪」
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"name":"大阪市","latitude":34.69,"longitude":135.5,"country":"日本","admin1":"大阪府"}]}`))
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

func TestWeatherTool(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var queried string
	server := fakeOpenMeteo(t, &queried)
	defer server.Close()
	client := weather.NewClient(server.URL+"/forecast", server.URL+"/search", server.Client())
	home := weather.FixedHome{Place: weather.Place{Name: "家", Latitude: 10.5, Longitude: 20.5}}

	box, _ := NewToolbox(ctx, nil, logger, WeatherTools(client, home, logger)...)
	result, err := box.Call(ctx, brain.ToolCall{Name: "get_weather"})
	if err != nil || !strings.Contains(result, `"weather":"小雨"`) || !strings.Contains(result, `"rain_chance_percent":80`) {
		t.Fatalf("查家里：%s %v", result, err)
	}
	if queried != "10.5000,20.5000,3" {
		t.Errorf("不说地点查家里、默认 3 天：%s", queried)
	}
	result, err = box.Call(ctx, brain.ToolCall{Name: "get_weather", Arguments: map[string]any{"place": "大阪", "days": 2}})
	if err != nil || !strings.Contains(result, "大阪市") || queried != "34.6900,135.5000,2" {
		t.Errorf("查大阪：%s %v %s", result, err, queried)
	}
	if _, err := box.Call(ctx, brain.ToolCall{Name: "get_weather", Arguments: map[string]any{"place": "不存在的地方"}}); err == nil {
		t.Error("找不到的地名应报错")
	}

	noHome, _ := NewToolbox(ctx, nil, logger, WeatherTools(client, nil, logger)...)
	if _, err := noHome.Call(ctx, brain.ToolCall{Name: "get_weather"}); err == nil || !strings.Contains(err.Error(), "HARO_WEATHER_HOME") {
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
