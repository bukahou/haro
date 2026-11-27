package weather

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openMeteoReplying 是只会回 body 的假 Open-Meteo。
func openMeteoReplying(t *testing.T, body string) *OpenMeteoClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(server.Close)
	return NewOpenMeteoClient(server.URL, server.URL, server.Client())
}

// 回复里是 null 的项不输出（不当成 0℃、降水 0% 报给模型）；某一天什么都没给就不列这一天。
func TestOpenMeteoLeavesOutMissingValues(t *testing.T) {
	client := openMeteoReplying(t, `{"current":{"time":"2026-11-21T10:00","temperature_2m":null,"apparent_temperature":null,
		"relative_humidity_2m":60,"weather_code":61,"wind_speed_10m":null},
		"daily":{"time":["2026-11-21","2026-11-22","2026-11-23"],"weather_code":[61,null,null],"temperature_2m_max":[15,null,null],
		"temperature_2m_min":[8,6,null],"precipitation_probability_max":[80,0,null],"precipitation_sum":[5.2,null,null]}}`)
	forecast, err := client.ForecastFor(context.Background(), sapporo, 3)
	if err != nil {
		t.Fatal(err)
	}
	current := forecast.Current
	if current.Temperature != nil || current.FeelsLike != nil || current.WindSpeed != nil || show(current.Humidity) != "60" ||
		current.Weather != "小雨" {
		t.Errorf("现在的天气：null 的不该有，有的照给：%+v", current)
	}
	if len(forecast.Days) != 2 {
		t.Fatalf("什么都没给的那天不列：%+v", forecast.Days)
	}
	second := forecast.Days[1]
	if second.Weather != "" || second.High != nil || second.RainMillimeters != nil || show(second.Low) != "6" || show(second.RainChancePercent) != "0" {
		t.Errorf("第二天：null 的不该有，真的 0 照给：%+v", second)
	}
	raw, _ := json.Marshal(forecast)
	for _, missing := range []string{`"temperature_c"`, `"feels_like_c"`, `"wind_kmh"`} {
		if strings.Contains(string(raw), missing) {
			t.Errorf("null 的项不应出现在给模型的结果里（%s）：%s", missing, raw)
		}
	}
}

// 现在的天气各项都是 null：当作没查到（报错），上层改用备用来源。
func TestOpenMeteoWithoutCurrentWeatherFallsBack(t *testing.T) {
	client := openMeteoReplying(t, `{"current":{"time":"2026-11-21T10:00","temperature_2m":null,"apparent_temperature":null,
		"relative_humidity_2m":null,"weather_code":null,"wind_speed_10m":null},
		"daily":{"time":["2026-11-21"],"weather_code":[0],"temperature_2m_max":[15],"temperature_2m_min":[8],
		"precipitation_probability_max":[10],"precipitation_sum":[0]}}`)
	if _, err := client.ForecastFor(context.Background(), sapporo, 1); !errors.Is(err, errNoCurrentWeather) {
		t.Fatalf("现在的天气整段都缺应报错：%v", err)
	}
	backup := &stubForecaster{forecast: Forecast{Source: "日本气象厅"}}
	forecast, err := WithBackup(client, "Open-Meteo", backup, "日本气象厅", slog.New(slog.NewTextHandler(io.Discard, nil))).
		ForecastFor(context.Background(), sapporo, 1)
	if err != nil || forecast.Source != "日本气象厅" || backup.calls != 1 {
		t.Errorf("应改用备用：%+v %v（备用被问了 %d 次）", forecast, err, backup.calls)
	}
}

// 第一次请求卡住（超时）、第二次正常：自动重试成功；服务报 4xx 不重试。
func TestRetriesOnceOnTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Query().Get("name") == "bad" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if n == 1 {
			select { // 第一次卡住
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"name":"测试市","latitude":1,"longitude":2}]}`))
	}))
	defer server.Close()
	client := NewOpenMeteoClient(server.URL, server.URL, server.Client())
	client.fetch.attemptTimeout = 100 * time.Millisecond

	places, err := client.FindPlaces(context.Background(), PlaceQuery{Name: "测试", Language: "ja"}, 5)
	if err != nil || len(places) != 1 || places[0].Name != "测试市" || calls.Load() != 2 {
		t.Fatalf("超时后应重试一次并成功：%+v %v 请求了 %d 次", places, err, calls.Load())
	}
	calls.Store(10)
	if _, err := client.FindPlaces(context.Background(), PlaceQuery{Name: "bad", Language: "ja"}, 5); err == nil || calls.Load() != 11 {
		t.Errorf("服务报 4xx 不重试：%v 请求了 %d 次", err, calls.Load()-10)
	}
}

// 配置里的地址自带查询参数（例如付费接口要的 apikey）：请求照样带上它，再加上这次的参数，不拼坏。
func TestOpenMeteoKeepsQueryInConfiguredURL(t *testing.T) {
	var got atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Query())
		_, _ = w.Write([]byte(`{"current":{"time":"2026-11-21T10:00","temperature_2m":12,"weather_code":0},
			"daily":{"time":["2026-11-21"],"weather_code":[0],"temperature_2m_max":[15],"temperature_2m_min":[8]}}`))
	}))
	t.Cleanup(server.Close)
	client := NewOpenMeteoClient(server.URL+"/v1/forecast?apikey=K", server.URL, server.Client())
	if _, err := client.ForecastFor(context.Background(), sapporo, 1); err != nil {
		t.Fatal(err)
	}
	query, _ := got.Load().(url.Values)
	if query.Get("apikey") != "K" || query.Get("latitude") == "" {
		t.Errorf("请求应同时带上地址自带的 apikey 和这次的参数：%v", query)
	}
}
