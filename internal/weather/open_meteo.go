// Package weather 查天气（v5 E2 / R7 c）：Open-Meteo 的公开 HTTP 接口，不用密钥，只发位置。
// Open-Meteo 没有官方 MCP 服务，接口又很简单 ⇒ 直接内置调用（R7：没有官方 MCP 的简单 HTTP 接口内置）。
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Place 是一个地点。
type Place struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"country,omitempty"`
	Region    string  `json:"region,omitempty"`
}

// Forecast 是给模型看的天气（已把天气代码翻成中文）。
type Forecast struct {
	Place   Place     `json:"place"`
	Current Current   `json:"current"`
	Days    []DayCast `json:"days"`
	Source  string    `json:"source"`
}

// Current 是现在的天气。
type Current struct {
	Time        string  `json:"time"`
	Weather     string  `json:"weather"`
	Temperature float64 `json:"temperature_c"`
	FeelsLike   float64 `json:"feels_like_c"`
	Humidity    float64 `json:"humidity_percent"`
	WindSpeed   float64 `json:"wind_kmh"`
}

// DayCast 是某一天的预报。
type DayCast struct {
	Date              string  `json:"date"`
	Weather           string  `json:"weather"`
	High              float64 `json:"high_c"`
	Low               float64 `json:"low_c"`
	RainChancePercent float64 `json:"rain_chance_percent"`
	RainMillimeters   float64 `json:"rain_mm"`
}

// Client 是 Open-Meteo 客户端。
type Client struct {
	forecastURL, geocodingURL string
	http                      *http.Client
	attemptTimeout            time.Duration // 每次请求最多等多久；超时或连不上时再试一次
}

// 每次请求最多等 3.5 秒，失败再试一次（两次合计 7 秒，留在大脑 10 秒的工具时限内）。
// 实测（2026-11-17）：平时 0.8 秒左右，偶尔有一次请求卡住到超时、紧接着再请求就正常。
const (
	defaultAttemptTimeout = 3500 * time.Millisecond
	maxAttempts           = 2
)

// NewClient 创建客户端；两个地址来自配置（默认官方公开地址）。
func NewClient(forecastURL, geocodingURL string, httpClient *http.Client) *Client {
	return &Client{forecastURL: forecastURL, geocodingURL: geocodingURL, http: httpClient, attemptTimeout: defaultAttemptTimeout}
}

// ErrPlaceNotFound 表示找不到这个地名。
var ErrPlaceNotFound = errors.New("找不到这个地方")

// FindPlace 按地名找坐标（日本的地名在日本里找；language 决定返回的名字用什么语言）。
func (c *Client) FindPlace(ctx context.Context, name, language string) (Place, error) {
	query := url.Values{"name": {name}, "count": {"1"}, "language": {language}, "format": {"json"}}
	var response struct {
		Results []struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Country   string  `json:"country"`
			Admin1    string  `json:"admin1"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, c.geocodingURL+"?"+query.Encode(), &response); err != nil {
		return Place{}, err
	}
	if len(response.Results) == 0 {
		return Place{}, fmt.Errorf("%w：%s", ErrPlaceNotFound, name)
	}
	found := response.Results[0]
	return Place{Name: found.Name, Latitude: found.Latitude, Longitude: found.Longitude, Country: found.Country, Region: found.Admin1}, nil
}

// ForecastFor 查一个地点现在的天气和未来 days 天（含今天，1–7）的预报，时间按当地时区。
func (c *Client) ForecastFor(ctx context.Context, place Place, days int) (Forecast, error) {
	if days < 1 {
		days = 1
	}
	if days > 7 {
		days = 7
	}
	query := url.Values{
		"latitude":      {strconv.FormatFloat(place.Latitude, 'f', 4, 64)},
		"longitude":     {strconv.FormatFloat(place.Longitude, 'f', 4, 64)},
		"current":       {"temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"},
		"daily":         {"weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,precipitation_sum"},
		"timezone":      {"auto"},
		"forecast_days": {strconv.Itoa(days)},
	}
	var response struct {
		Current struct {
			Time        string  `json:"time"`
			Temperature float64 `json:"temperature_2m"`
			FeelsLike   float64 `json:"apparent_temperature"`
			Humidity    float64 `json:"relative_humidity_2m"`
			WeatherCode int     `json:"weather_code"`
			WindSpeed   float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			Time        []string  `json:"time"`
			WeatherCode []int     `json:"weather_code"`
			High        []float64 `json:"temperature_2m_max"`
			Low         []float64 `json:"temperature_2m_min"`
			RainChance  []float64 `json:"precipitation_probability_max"`
			RainSum     []float64 `json:"precipitation_sum"`
		} `json:"daily"`
	}
	if err := c.getJSON(ctx, c.forecastURL+"?"+query.Encode(), &response); err != nil {
		return Forecast{}, err
	}
	forecast := Forecast{Place: place, Source: "Open-Meteo", Current: Current{
		Time: response.Current.Time, Weather: Describe(response.Current.WeatherCode), Temperature: response.Current.Temperature,
		FeelsLike: response.Current.FeelsLike, Humidity: response.Current.Humidity, WindSpeed: response.Current.WindSpeed}}
	daily := response.Daily
	for i, date := range daily.Time {
		day := DayCast{Date: date}
		if i < len(daily.WeatherCode) {
			day.Weather = Describe(daily.WeatherCode[i])
		}
		day.High, day.Low = at(daily.High, i), at(daily.Low, i)
		day.RainChancePercent, day.RainMillimeters = at(daily.RainChance, i), at(daily.RainSum, i)
		forecast.Days = append(forecast.Days, day)
	}
	return forecast, nil
}

func at(values []float64, i int) float64 {
	if i < len(values) {
		return values[i]
	}
	return 0
}

// getJSON 请求一次 JSON；超时或连不上时再试一次（服务报错、回复看不懂不重试）。
func (c *Client) getJSON(ctx context.Context, address string, target any) error {
	timeout := c.attemptTimeout
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var retry bool
		retry, err = c.getJSONOnce(ctx, address, target, timeout)
		if err == nil || !retry || ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf("%w（试了 %d 次）", err, maxAttempts)
}

func (c *Client) getJSONOnce(ctx context.Context, address string, target any, timeout time.Duration) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return true, fmt.Errorf("天气服务连不上: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode >= 500, fmt.Errorf("天气服务报错: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return ctx.Err() != nil, fmt.Errorf("天气服务的回复看不懂: %w", err)
	}
	return false, nil
}

// Describe 把 WMO 天气代码翻成中文。
func Describe(code int) string {
	switch {
	case code == 0:
		return "晴"
	case code == 1:
		return "基本晴"
	case code == 2:
		return "多云"
	case code == 3:
		return "阴"
	case code == 45 || code == 48:
		return "雾"
	case code >= 51 && code <= 57:
		return "毛毛雨"
	case code == 61 || code == 80:
		return "小雨"
	case code == 63 || code == 81:
		return "中雨"
	case code == 65 || code == 82:
		return "大雨"
	case code == 66 || code == 67:
		return "冻雨"
	case code == 71 || code == 85:
		return "小雪"
	case code == 73:
		return "中雪"
	case code == 75 || code == 86:
		return "大雪"
	case code == 77:
		return "米雪"
	case code == 95:
		return "雷雨"
	case code == 96 || code == 99:
		return "雷雨伴冰雹"
	default:
		return "天气代码 " + strconv.Itoa(code)
	}
}
