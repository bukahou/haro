//go:build integration

// 对真实的气象厅、国土地理院、Open-Meteo 接口的冒烟测试（默认不跑，CI 也不跑；都不要密钥、不花钱）：
//
//	go test -tags integration -run Live -v -count=1 ./internal/weather/
//
// 验证：地点 → 预报地区（含北海道、冲绳这种一个县分几个预报区的，政令市的区），真实预报能合成 7 天，实况能取到；
// Open-Meteo 为主、气象厅备用时谁答的、花了多久。
package weather

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

const (
	liveJMAURL      = "https://www.jma.go.jp/bosai"
	liveGeocoderURL = "https://mreversegeocoder.gsi.go.jp/reverse-geocoder/LonLatToAddress"
)

var livePlaces = []Place{
	{Name: "東京都庁", Latitude: 35.6895, Longitude: 139.6917},
	{Name: "札幌駅", Latitude: 43.0687, Longitude: 141.3508},
	{Name: "大阪駅", Latitude: 34.7025, Longitude: 135.4959},
	{Name: "那覇市役所", Latitude: 26.2124, Longitude: 127.6809},
}

func TestJMALive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	jma := NewJMA(liveJMAURL, liveGeocoderURL, NewHTTPClient())
	for _, place := range livePlaces {
		started := time.Now()
		forecast, err := jma.ForecastFor(ctx, place, 7)
		if err != nil {
			t.Errorf("%s：%v", place.Name, err)
			continue
		}
		t.Logf("%s → %s（%v）", place.Name, forecast.Area, time.Since(started).Round(time.Millisecond))
		t.Logf("  实况 %s：气温 %s、湿度 %s、风 %s km/h", forecast.Current.Time, show(forecast.Current.Temperature),
			show(forecast.Current.Humidity), show(forecast.Current.WindSpeed))
		for _, day := range forecast.Days {
			t.Logf("  %s %-12s 最高 %s 最低 %s 降水概率 %s", day.Date, day.Weather, show(day.High), show(day.Low), show(day.RainChancePercent))
		}
		if len(forecast.Days) < 6 || forecast.Days[0].Weather == "" || forecast.Current.Temperature == nil {
			t.Errorf("%s：预报应有 6–7 天、今天有天气、实况有气温", place.Name)
		}
	}
}

func TestBackupLive(t *testing.T) {
	// 模拟真实遇到过的故障：TCP 能连上，TLS 握手一直等不到回复
	hanging, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hanging.Close()
	go func() {
		for {
			conn, err := hanging.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, primary := range []struct{ name, forecastURL string }{
		{"真实的 Open-Meteo", "https://api.open-meteo.com/v1/forecast"},
		{"握手卡住的主力（模拟）", "https://" + hanging.Addr().String() + "/v1/forecast"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		http := NewHTTPClient()
		openMeteo := NewClient(primary.forecastURL, "https://geocoding-api.open-meteo.com/v1/search", http)
		forecaster := WithBackup(openMeteo, "Open-Meteo", NewJMA(liveJMAURL, liveGeocoderURL, http), "日本气象厅", logger)
		started := time.Now()
		forecast, err := forecaster.ForecastFor(ctx, livePlaces[0], 3)
		cancel()
		if err != nil {
			t.Errorf("%s：%v", primary.name, err)
			continue
		}
		t.Logf("%s：回答的来源 %s，用时 %v；说明：%s", primary.name, forecast.Source, time.Since(started).Round(time.Millisecond), forecast.Note)
	}
}
