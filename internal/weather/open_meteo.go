package weather

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Client 是 Open-Meteo 客户端：按坐标查天气（全球），按地名找坐标。
type Client struct {
	forecastURL, geocodingURL string
	fetch                     fetcher
}

// NewClient 创建客户端；两个地址来自配置（默认官方公开地址）。
func NewClient(forecastURL, geocodingURL string, httpClient *http.Client) *Client {
	return &Client{forecastURL: forecastURL, geocodingURL: geocodingURL,
		fetch: fetcher{http: httpClient, attemptTimeout: defaultAttemptTimeout, service: "天气服务（Open-Meteo）"}}
}

// FindPlaces 按地名找坐标：返回地名库里最多 limit 个候选（最相关的在前），一个都没有时返回空。
// language 决定返回的名字用什么语言写。
func (c *Client) FindPlaces(ctx context.Context, name, language string, limit int) ([]Place, error) {
	query := url.Values{"name": {name}, "count": {strconv.Itoa(limit)}, "language": {language}, "format": {"json"}}
	var response struct {
		Results []struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Country   string  `json:"country"`
			Admin1    string  `json:"admin1"`
		} `json:"results"`
	}
	if err := c.fetch.getJSON(ctx, c.geocodingURL+"?"+query.Encode(), &response); err != nil {
		return nil, err
	}
	places := make([]Place, 0, len(response.Results))
	for _, found := range response.Results {
		places = append(places, Place{Name: found.Name, Latitude: found.Latitude, Longitude: found.Longitude,
			Country: found.Country, Region: found.Admin1})
	}
	return places, nil
}

// ForecastFor 实现 Forecaster：现在的天气和未来 days 天（含今天，1–7）的预报，时间按当地时区。
func (c *Client) ForecastFor(ctx context.Context, place Place, days int) (Forecast, error) {
	query := url.Values{
		"latitude":      {strconv.FormatFloat(place.Latitude, 'f', 4, 64)},
		"longitude":     {strconv.FormatFloat(place.Longitude, 'f', 4, 64)},
		"current":       {"temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"},
		"daily":         {"weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,precipitation_sum"},
		"timezone":      {"auto"},
		"forecast_days": {strconv.Itoa(clampDays(days))},
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
	if err := c.fetch.getJSON(ctx, c.forecastURL+"?"+query.Encode(), &response); err != nil {
		return Forecast{}, err
	}
	now := response.Current
	forecast := Forecast{Place: place, Source: "Open-Meteo", Current: Current{
		Time: now.Time, Weather: Describe(now.WeatherCode), Temperature: ptr(now.Temperature),
		FeelsLike: ptr(now.FeelsLike), Humidity: ptr(now.Humidity), WindSpeed: ptr(now.WindSpeed)}}
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

func at(values []float64, i int) *float64 {
	if i < len(values) {
		return ptr(values[i])
	}
	return nil
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
