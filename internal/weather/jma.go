package weather

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JMA 是日本气象厅网站数据（bosai）的天气来源：只覆盖日本，按地区预报，不用密钥。
// 这些数据文件是气象厅网站自己用的，不是正式公开的接口，格式可能不打招呼就变 ⇒ 只做备用。
// 地点 → 预报地区：国土地理院的地址反查（坐标 → 市町村）+ 气象厅的地区表；实况取最近的测气温的观测站。
type JMA struct {
	baseURL, geocoderURL string
	fetch, geocode       fetcher
	now                  func() time.Time

	// tables 是地区表：第一次用时下载，之后一直用（很少变）；同时要的只下载一次，不在锁里下载（下载要好几秒，
	// 查已经缓存的地区不该等它）；下载失败后一分钟内不再下载（见 cachedLookup）
	tables *cachedLookup[*jmaTables]
	mu     sync.Mutex
	areas  map[string]JMAArea
}

// jst 是气象厅数据用的时区（日本没有夏令时）。
var jst = time.FixedZone("JST", 9*60*60)

// NewJMA 创建气象厅来源；baseURL 是 bosai 数据的根地址，geocoderURL 是国土地理院的地址反查（都来自配置）。
func NewJMA(baseURL, geocoderURL string, httpClient *http.Client) *JMA {
	jma := &JMA{baseURL: strings.TrimRight(baseURL, "/"), geocoderURL: geocoderURL,
		fetch:   fetcher{http: httpClient, attemptTimeout: defaultAttemptTimeout, service: "气象厅"},
		geocode: fetcher{http: httpClient, attemptTimeout: defaultAttemptTimeout, service: "地址反查（国土地理院）"},
		now:     time.Now, areas: map[string]JMAArea{}}
	jma.tables = newCachedLookup(jma.downloadTables, 0)
	return jma
}

// ForecastFor 实现 Forecaster：短期预报（三天，天气是日语原文）+ 周间预报（到第 7 天）+ 最近观测站的实况。
func (j *JMA) ForecastFor(ctx context.Context, place Place, days int) (Forecast, error) {
	area, err := j.Locate(ctx, place)
	if err != nil {
		return Forecast{}, err
	}
	var reports []jmaReport
	if err := j.fetch.getJSON(ctx, j.baseURL+"/forecast/data/forecast/"+area.Office+".json", &reports); err != nil {
		return Forecast{}, err
	}
	forecast := Forecast{Place: place, Area: area.Describe(), Source: "日本气象厅",
		Days: jmaDays(reports, area, j.now(), clampDays(days)),
		Note: "这是按地区的预报，天气描述是气象厅的日语原文；没有体感温度，也没有降水量预报。"}
	if len(forecast.Days) == 0 {
		return Forecast{}, fmt.Errorf("气象厅的预报里没有%s的数据", area.Describe())
	}
	if current, err := j.observe(ctx, area); err != nil {
		forecast.Note += "现在的实况没取到（" + err.Error() + "）。"
	} else {
		forecast.Current = current
	}
	return forecast, nil
}

// jmaReport 是预报文件里的一份预报：文件里依次是短期预报、周间预报。
type jmaReport struct {
	TimeSeries []struct {
		TimeDefines []string        `json:"timeDefines"`
		Areas       []jmaSeriesArea `json:"areas"`
	} `json:"timeSeries"`
}

type jmaSeriesArea struct {
	Area struct {
		Code string `json:"code"`
	} `json:"area"`
	Weathers     []string `json:"weathers"`     // 短期：天气（日语原文）
	WeatherCodes []string `json:"weatherCodes"` // 短期与周间都有
	Pops         []string `json:"pops"`         // 短期：每 6 小时；周间：每天
	Temps        []string `json:"temps"`        // 短期：0 时那格是最低，9 时那格是最高
	TempsMin     []string `json:"tempsMin"`     // 周间
	TempsMax     []string `json:"tempsMax"`     // 周间
}

// jmaDays 把短期与周间预报合成逐日预报（今天起 days 天）。同一天两边都有时用短期的：更新、更细。
func jmaDays(reports []jmaReport, area JMAArea, now time.Time, days int) []DayCast {
	byDate := map[string]*DayCast{}
	day := func(define string) (*DayCast, time.Time, bool) {
		at, err := time.Parse(time.RFC3339, define)
		if err != nil {
			return nil, at, false
		}
		at = at.In(jst)
		date := at.Format(time.DateOnly)
		if byDate[date] == nil {
			byDate[date] = &DayCast{Date: date}
		}
		return byDate[date], at, true
	}
	for _, report := range reports {
		for _, series := range report.TimeSeries {
			for _, a := range series.Areas {
				code := a.Area.Code
				switch {
				case len(a.Weathers) > 0 && code == area.Class10: // 短期：天气
					for i, define := range series.TimeDefines {
						if d, _, ok := day(define); ok && d.Weather == "" && i < len(a.Weathers) {
							d.Weather = strings.ReplaceAll(strings.TrimSpace(a.Weathers[i]), "　", "")
						}
					}
				case len(a.Pops) > 0 && len(a.WeatherCodes) == 0 && code == area.Class10: // 短期：每 6 小时的降水概率 → 当天最大
					for i, define := range series.TimeDefines {
						d, _, ok := day(define)
						if value := forecastCell(a.Pops, i); ok && value != nil && (d.RainChancePercent == nil || *value > *d.RainChancePercent) {
							d.RainChancePercent = value
						}
					}
				case len(a.Temps) > 0 && code == area.TempPoint: // 短期：气温
					shortTermTemperatures(series.TimeDefines, a.Temps, day)
				case len(a.WeatherCodes) > 0 && len(a.Weathers) == 0 && code == area.WeekArea: // 周间：天气代码 + 降水概率
					for i, define := range series.TimeDefines {
						d, _, ok := day(define)
						if !ok {
							continue
						}
						if d.Weather == "" && i < len(a.WeatherCodes) {
							d.Weather = jmaWeatherName(a.WeatherCodes[i])
						}
						if d.RainChancePercent == nil {
							d.RainChancePercent = forecastCell(a.Pops, i)
						}
					}
				case (len(a.TempsMin) > 0 || len(a.TempsMax) > 0) && code == area.WeekTempPoint: // 周间：气温
					for i, define := range series.TimeDefines {
						if d, _, ok := day(define); ok {
							d.High = cmp.Or(d.High, forecastCell(a.TempsMax, i))
							d.Low = cmp.Or(d.Low, forecastCell(a.TempsMin, i))
						}
					}
				}
			}
		}
	}
	start := now.In(jst)
	var result []DayCast
	for i := range days {
		if d := byDate[start.AddDate(0, 0, i).Format(time.DateOnly)]; d != nil {
			result = append(result, *d)
		}
	}
	return result
}

// shortTermTemperatures：0 时那格是当天最低、9 时那格是当天最高。当天上午以后发布的预报里，当天最低那格已经过去，
// 填的是和最高一样的值 ⇒ 同一天最低等于最高时不报最低。
func shortTermTemperatures(defines, temps []string, day func(string) (*DayCast, time.Time, bool)) {
	lows, highs := map[*DayCast]*float64{}, map[*DayCast]*float64{}
	for i, define := range defines {
		d, at, ok := day(define)
		value := forecastCell(temps, i)
		if !ok || value == nil {
			continue
		}
		if at.Hour() < 9 {
			lows[d] = value
		} else {
			highs[d] = value
		}
	}
	for d, high := range highs {
		d.High = cmp.Or(d.High, high)
	}
	for d, low := range lows {
		if high := highs[d]; high != nil && *high == *low {
			continue
		}
		d.Low = cmp.Or(d.Low, low)
	}
}

// observe 取附近观测站最新的实况（每 10 分钟更新；每个站按 3 小时一个文件）。
// 观测站会缺测（例如维修中，气温为 null）⇒ 最近的站没有气温时改看下一个；都没有气温时用最近那个站有的数据。
func (j *JMA) observe(ctx context.Context, area JMAArea) (Current, error) {
	latest, err := j.fetch.getText(ctx, j.baseURL+"/amedas/data/latest_time.txt")
	if err != nil {
		return Current{}, err
	}
	at, err := time.Parse(time.RFC3339, latest)
	if err != nil {
		return Current{}, fmt.Errorf("气象厅实况的时间看不懂：%q", latest)
	}
	at = at.In(jst)
	var fallback *Current
	for _, station := range area.Stations {
		current, err := j.observeStation(ctx, station, at)
		if err != nil {
			continue
		}
		if current.Temperature != nil {
			return current, nil
		}
		if fallback == nil {
			fallback = &current
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return Current{}, errors.New("附近的观测站都没有数据")
}

// observeStation 取一个观测站在 at（或文件里最新那一刻）的实况。
func (j *JMA) observeStation(ctx context.Context, station JMAStation, at time.Time) (Current, error) {
	block := at.Format("20060102") + fmt.Sprintf("_%02d", at.Hour()/3*3)
	var points map[string]jmaObservation
	if err := j.fetch.getJSON(ctx, j.baseURL+"/amedas/data/point/"+station.Code+"/"+block+".json", &points); err != nil {
		return Current{}, err
	}
	key := at.Format("20060102150405")
	if _, ok := points[key]; !ok && len(points) > 0 { // 最新那一刻还没进这个观测站的文件：用文件里最新的一条
		keys := make([]string, 0, len(points))
		for k := range points {
			keys = append(keys, k)
		}
		key = slices.Max(keys)
	}
	observation, ok := points[key]
	if !ok {
		return Current{}, errors.New("观测站没有数据")
	}
	observedAt, err := time.ParseInLocation("20060102150405", key, jst)
	if err != nil {
		return Current{}, fmt.Errorf("观测时间看不懂：%q", key)
	}
	current := Current{Time: observedAt.Format("2006-01-02T15:04"), Station: station.Name + "观测站",
		Temperature: observation.Temp.value(), Humidity: observation.Humidity.value(), RainLastHour: observation.Precipitation1h.value()}
	if wind := observation.Wind.value(); wind != nil { // 观测是米 / 秒，和 Open-Meteo 一样报公里 / 小时
		current.WindSpeed = new(math.Round(*wind*3.6*10) / 10)
	}
	return current, nil
}

type jmaObservation struct {
	Temp            jmaValue `json:"temp"`
	Humidity        jmaValue `json:"humidity"`
	Wind            jmaValue `json:"wind"`
	Precipitation1h jmaValue `json:"precipitation1h"`
}

// jmaValue 是一项观测：[值, 品质标志]；没测到时值是 null。
type jmaValue []*float64

func (v jmaValue) value() *float64 {
	if len(v) == 0 {
		return nil
	}
	return v[0]
}

// jmaWeatherName 把天气代码翻成文字（日语原文）；表里没有的照写代码。
func jmaWeatherName(code string) string {
	if numeric, err := strconv.Atoi(code); err == nil {
		if name, ok := jmaWeatherNames[numeric]; ok {
			return name
		}
	}
	return "天气代码 " + code
}

// forecastCell 取预报表第 i 格的数值；空字符串（这一格没有预报）为 nil。
func forecastCell(values []string, i int) *float64 {
	if i >= len(values) {
		return nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(values[i]), 64)
	if err != nil {
		return nil
	}
	return &value
}
