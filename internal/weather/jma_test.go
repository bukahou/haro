package weather

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 测试用的假数据照气象厅、国土地理院的真实格式缩写而成（东京、札幌、仙台；观测站 99999 是虚构的）。
const (
	fakeAreaTable = `{"offices":{"130000":{"name":"東京都"},"016000":{"name":"石狩・空知・後志地方"},"040000":{"name":"宮城県"}},
"class10s":{"130010":{"name":"東京地方","parent":"130000"},"016010":{"name":"石狩地方","parent":"016000"},
  "040010":{"name":"東部","parent":"040000"},"040020":{"name":"西部","parent":"040000"}},
"class15s":{"130011":{"name":"23区西部","parent":"130010"},"016012":{"name":"札幌地方","parent":"016010"},
  "040011":{"name":"仙台地域","parent":"040010"},"040021":{"name":"白石地域","parent":"040020"}},
"class20s":{"0110000":{"name":"札幌市","parent":"016012"},"0120200":{"name":"函館市","parent":"016012"},
  "0410001":{"name":"仙台市東部","parent":"040011"},"0410002":{"name":"仙台市西部","parent":"040021"},
  "1310100":{"name":"千代田区","parent":"130011"},"1310400":{"name":"新宿区","parent":"130011"}}}`
	fakeForecastAreas = `{"130000":[{"class10":"130010","amedas":["44132"],"class20":"1310100"}],
"016000":[{"class10":"016010","amedas":["14163"],"class20":"0110000"}],
"040000":[{"class10":"040010","amedas":["34392"],"class20":"0410001"},{"class10":"040020","amedas":["34461"],"class20":"0410002"}]}`
	fakeWeekAreas = `{"130000":[{"srf":"130010","week":"130010","amedas":"44132"}],
"016000":[{"srf":"016010","week":"016000","amedas":"14163"}],
"040000":[{"srf":"040010","week":"040010","amedas":"34392"},{"srf":"040020","week":"040020","amedas":"34461"}]}`
	fakeStations = `{"44132":{"elems":"11111111","lat":[35,41.5],"lon":[139,45.0],"kjName":"東京"},
"44100":{"elems":"01000000","lat":[35,41.4],"lon":[139,41.5],"kjName":"雨量だけの観測所"},
"99999":{"elems":"11110000","lat":[35,40.0],"lon":[139,40.0],"kjName":"テスト観測所"},
"14163":{"elems":"11111111","lat":[43,3.6],"lon":[141,19.7],"kjName":"札幌"},
"34392":{"elems":"11111111","lat":[38,15.7],"lon":[140,53.8],"kjName":"仙台"},
"34461":{"elems":"11111111","lat":[38,0.1],"lon":[140,37.0],"kjName":"白石"}}`
	// 11:00 发布的东京预报：短期（今明后三天）+ 周间（明天起）。当天最低那格（0 时）已过去，填的是最高 20。
	fakeTokyoForecast = `[{"reportDatetime":"2026-11-18T11:00:00+09:00","timeSeries":[
 {"timeDefines":["2026-11-18T11:00:00+09:00","2026-11-19T00:00:00+09:00","2026-11-20T00:00:00+09:00"],
  "areas":[{"area":{"name":"東京地方","code":"130010"},"weatherCodes":["101","200","300"],"weathers":["晴れ　時々　くもり","くもり","雨"]},
           {"area":{"name":"伊豆諸島北部","code":"130020"},"weatherCodes":["100","100","100"],"weathers":["晴れ","晴れ","晴れ"]}]},
 {"timeDefines":["2026-11-18T12:00:00+09:00","2026-11-18T18:00:00+09:00","2026-11-19T00:00:00+09:00",
                 "2026-11-19T06:00:00+09:00","2026-11-19T12:00:00+09:00","2026-11-19T18:00:00+09:00"],
  "areas":[{"area":{"name":"東京地方","code":"130010"},"pops":["10","0","20","40","30","0"]}]},
 {"timeDefines":["2026-11-18T09:00:00+09:00","2026-11-18T00:00:00+09:00","2026-11-19T00:00:00+09:00","2026-11-19T09:00:00+09:00"],
  "areas":[{"area":{"name":"東京","code":"44132"},"temps":["20","20","12","19"]}]}]},
{"reportDatetime":"2026-11-18T11:00:00+09:00","timeSeries":[
 {"timeDefines":["2026-11-19T00:00:00+09:00","2026-11-20T00:00:00+09:00","2026-11-21T00:00:00+09:00","2026-11-22T00:00:00+09:00"],
  "areas":[{"area":{"name":"東京地方","code":"130010"},"weatherCodes":["200","300","101","999"],"pops":["","70","10","20"]}]},
 {"timeDefines":["2026-11-19T00:00:00+09:00","2026-11-20T00:00:00+09:00","2026-11-21T00:00:00+09:00","2026-11-22T00:00:00+09:00"],
  "areas":[{"area":{"name":"東京","code":"44132"},"tempsMin":["","9","6","5"],"tempsMax":["","15","14","13"]}]}]}]`
	// 15:20 时湿度没测到（null）。
	fakeObservations = `{"20261118151000":{"temp":[18.0,0],"humidity":[70,0],"wind":[2.0,0],"precipitation1h":[0.0,0]},
"20261118152000":{"temp":[18.2,0],"humidity":[null,5],"wind":[2.8,0],"precipitation1h":[0.5,0]}}`
)

var (
	shinjuku = Place{Name: "新宿", Latitude: 35.6895, Longitude: 139.6917}
	sapporo  = Place{Name: "札幌", Latitude: 43.0621, Longitude: 141.3544}
	sendai   = Place{Name: "仙台", Latitude: 38.2682, Longitude: 140.8694}
	atSea    = Place{Name: "海上", Latitude: 30, Longitude: 140}
	paris    = Place{Name: "Paris", Latitude: 48.8566, Longitude: 2.3522}
)

type fakeJMAServer struct {
	*httptest.Server
	geocodes       atomic.Int32
	areaTables     atomic.Int32                  // 地区表（area.json）被下载了几次
	tableGate      atomic.Pointer[chan struct{}] // 设了就等它关上才回地区表
	geocodeGate    atomic.Pointer[chan struct{}] // 设了就等它关上才回地址反查
	latestTime     atomic.Value                  // string
	nearestMissing atomic.Bool                   // 最近的观测站缺测（气温为 null）
}

// 第二近的观测站（東京）的实况。
const fakeObservationsTokyo = `{"20261118152000":{"temp":[17.9,0],"humidity":[66,0],"wind":[1.5,0],"precipitation1h":[0.0,0]}}`

// 缺测的观测站：气温、湿度为 null（品质标志 5），只有风。
const fakeObservationsMissing = `{"20261118152000":{"temp":[null,5],"humidity":[null,5],"wind":[3.0,0],"precipitation1h":[null,5]}}`

func newFakeJMAServer(t *testing.T) *fakeJMAServer {
	t.Helper()
	server := &fakeJMAServer{}
	server.latestTime.Store("2026-11-18T15:20:00+09:00")
	files := map[string]string{
		"/bosai/common/const/area.json":                   fakeAreaTable,
		"/bosai/forecast/const/forecast_area.json":        fakeForecastAreas,
		"/bosai/forecast/const/week_area.json":            fakeWeekAreas,
		"/bosai/amedas/const/amedastable.json":            fakeStations,
		"/bosai/forecast/data/forecast/130000.json":       fakeTokyoForecast,
		"/bosai/amedas/data/point/44132/20261118_15.json": fakeObservationsTokyo,
	}
	municipalities := map[string]string{"35.6895": "13104", "43.0621": "1101", "38.2682": "04101"} // 札幌这条故意少了开头的 0
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/geocoder":
			server.geocodes.Add(1)
			if gate := server.geocodeGate.Load(); gate != nil {
				<-*gate
			}
			if code, ok := municipalities[r.URL.Query().Get("lat")]; ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"results": map[string]string{"muniCd": code, "lv01Nm": "－"}})
				return
			}
			_, _ = w.Write([]byte(`{}`)) // 海上
		case r.URL.Path == "/bosai/amedas/data/latest_time.txt":
			_, _ = w.Write([]byte(server.latestTime.Load().(string)))
		case r.URL.Path == "/bosai/amedas/data/point/99999/20261118_15.json":
			if server.nearestMissing.Load() {
				_, _ = w.Write([]byte(fakeObservationsMissing))
				return
			}
			_, _ = w.Write([]byte(fakeObservations))
		case files[r.URL.Path] != "":
			if r.URL.Path == "/bosai/common/const/area.json" {
				server.areaTables.Add(1)
				if gate := server.tableGate.Load(); gate != nil {
					<-*gate
				}
			}
			_, _ = w.Write([]byte(files[r.URL.Path]))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *fakeJMAServer) client() *JMA {
	jma := NewJMA(s.URL+"/bosai/", s.URL+"/geocoder", s.Client())
	jma.now = func() time.Time { return time.Date(2026, 11, 18, 15, 25, 0, 0, jst) }
	return jma
}

func TestJMALocate(t *testing.T) {
	server := newFakeJMAServer(t)
	jma := server.client()
	ctx := context.Background()
	cases := []struct {
		name                                            string
		place                                           Place
		office, class10, temp, week, weekTemp, stations string
	}{
		// 实况：附近测气温的站，近的在前，40 公里以外的不要；只测雨量的站不算
		{"普通的区", shinjuku, "130000", "130010", "44132", "130010", "44132", "99999 44132"},
		{"政令市的区：归到所属的市；市町村代码补齐 5 位", sapporo, "016000", "016010", "14163", "016000", "14163", "14163"},
		{"市被拆成两块：选代表气温点更近的那块", sendai, "040000", "040010", "34392", "040010", "34392", "34392 34461"},
	}
	for _, c := range cases {
		area, err := jma.Locate(ctx, c.place)
		if err != nil {
			t.Errorf("%s：%v", c.name, err)
			continue
		}
		var stations []string
		for _, station := range area.Stations {
			stations = append(stations, station.Code)
		}
		got := []string{area.Office, area.Class10, area.TempPoint, area.WeekArea, area.WeekTempPoint, strings.Join(stations, " ")}
		want := []string{c.office, c.class10, c.temp, c.week, c.weekTemp, c.stations}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s：得到 %v，应为 %v", c.name, got, want)
		}
	}
	if area, _ := jma.Locate(ctx, shinjuku); area.Describe() != "東京都 東京地方" {
		t.Errorf("地区说明：%q", area.Describe())
	}

	asked := server.geocodes.Load()
	if _, err := jma.Locate(ctx, shinjuku); err != nil || server.geocodes.Load() != asked {
		t.Errorf("同一个地点应用缓存，不再反查：%v，反查了 %d 次", err, server.geocodes.Load()-asked)
	}
	if _, err := jma.Locate(ctx, atSea); !errors.Is(err, ErrNotCovered) {
		t.Errorf("海上应报不覆盖：%v", err)
	}
	asked = server.geocodes.Load()
	if _, err := jma.Locate(ctx, paris); !errors.Is(err, ErrNotCovered) || server.geocodes.Load() != asked {
		t.Errorf("日本以外应直接报不覆盖、不去反查：%v", err)
	}
}

func TestJMAForecast(t *testing.T) {
	server := newFakeJMAServer(t)
	jma := server.client()
	forecast, err := jma.ForecastFor(context.Background(), shinjuku, 5)
	if err != nil {
		t.Fatal(err)
	}
	type day struct {
		date, weather          string
		high, low, rainChances string
	}
	want := []day{
		{"2026-11-18", "晴れ時々くもり", "20", "-", "10"},  // 全角空格去掉；最低那格是占位，不报；降水概率取 6 小时里最大
		{"2026-11-19", "くもり", "19", "12", "40"},     // 短期优先于周间
		{"2026-11-20", "雨", "15", "9", "70"},        // 气温和降水概率来自周间
		{"2026-11-21", "晴時々曇", "14", "6", "10"},     // 周间只有代码：查表
		{"2026-11-22", "天气代码 999", "13", "5", "20"}, // 表里没有的代码
	}
	if len(forecast.Days) != len(want) {
		t.Fatalf("应有 %d 天：%+v", len(want), forecast.Days)
	}
	for i, w := range want {
		d := forecast.Days[i]
		got := day{d.Date, d.Weather, show(d.High), show(d.Low), show(d.RainChancePercent)}
		if got != w {
			t.Errorf("第 %d 天：得到 %+v，应为 %+v", i+1, got, w)
		}
	}
	current := forecast.Current
	if current.Time != "2026-11-18T15:20" || current.Station != "テスト観測所观测站" || show(current.Temperature) != "18.2" ||
		current.Humidity != nil || show(current.WindSpeed) != "10.1" || show(current.RainLastHour) != "0.5" || current.FeelsLike != nil {
		t.Errorf("实况（湿度没测到不报；风速换成公里 / 小时）：%+v", current)
	}
	if forecast.Source != "日本气象厅" || !strings.Contains(forecast.Area, "東京地方") || !strings.Contains(forecast.Note, "日语原文") {
		t.Errorf("来源与说明：%+v", forecast)
	}
	if raw, _ := json.Marshal(forecast); strings.Contains(string(raw), "feels_like") || strings.Contains(string(raw), "rain_mm") {
		t.Errorf("没有的项不应出现：%s", raw)
	}

	if days, _ := jma.ForecastFor(context.Background(), shinjuku, 2); len(days.Days) != 2 {
		t.Errorf("只要 2 天：%+v", days.Days)
	}
	server.nearestMissing.Store(true) // 最近的观测站缺测：改看下一个
	if missing, err := jma.ForecastFor(context.Background(), shinjuku, 1); err != nil ||
		missing.Current.Station != "東京观测站" || show(missing.Current.Temperature) != "17.9" {
		t.Errorf("最近的站缺测时应改用下一个站：%+v %v", missing.Current, err)
	}
	server.nearestMissing.Store(false)
	server.latestTime.Store("2026-11-18T15:30:00+09:00") // 最新那一刻还没进观测站的文件
	if later, err := jma.ForecastFor(context.Background(), shinjuku, 1); err != nil || later.Current.Time != "2026-11-18T15:20" {
		t.Errorf("用文件里最新的一条：%+v %v", later.Current, err)
	}
	server.latestTime.Store("看不懂")
	if broken, err := jma.ForecastFor(context.Background(), shinjuku, 1); err != nil || broken.Current.Time != "" ||
		!strings.Contains(broken.Note, "实况没取到") || len(broken.Days) != 1 {
		t.Errorf("实况取不到时照样给预报：%+v %v", broken, err)
	}
}

func show(value *float64) string {
	if value == nil {
		return "-"
	}
	raw, _ := json.Marshal(*value)
	return string(raw)
}

// 地区表要下载好几秒：同时查两处只下载一次；查已经缓存的地区不等别处的下载、地址反查。
func TestJMALocateDoesNotHoldTheLockWhileFetching(t *testing.T) {
	server := newFakeJMAServer(t)
	jma := server.client()
	ctx := context.Background()

	gate := make(chan struct{})
	server.tableGate.Store(&gate)
	results := make(chan error, 2)
	for _, place := range []Place{shinjuku, sendai} {
		go func() { _, err := jma.Locate(ctx, place); results <- err }()
	}
	time.Sleep(100 * time.Millisecond) // 两个都在等地区表
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if n := server.areaTables.Load(); n != 1 {
		t.Errorf("同时查两处，地区表应只下载一次：%d 次", n)
	}

	geocodeGate := make(chan struct{})
	server.geocodeGate.Store(&geocodeGate)
	go func() { _, err := jma.Locate(ctx, sapporo); results <- err }() // 卡在地址反查
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	if area, err := jma.Locate(ctx, shinjuku); err != nil || area.Office != "130000" || time.Since(started) > 50*time.Millisecond {
		t.Errorf("已经缓存的地区应马上返回：%+v %v 用了 %v", area, err, time.Since(started))
	}
	close(geocodeGate)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
}
