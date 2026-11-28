package weather

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bukahou/haro/internal/endpoint"
)

// OpenMeteoClient 是 Open-Meteo 客户端：按坐标查天气（全球），按地名找坐标。
type OpenMeteoClient struct {
	forecastURL, geocodingURL string
	fetch                     fetcher
}

// NewOpenMeteoClient 创建客户端；两个地址来自配置（默认官方公开地址）。
func NewOpenMeteoClient(forecastURL, geocodingURL string, httpClient *http.Client) *OpenMeteoClient {
	return &OpenMeteoClient{forecastURL: forecastURL, geocodingURL: geocodingURL,
		fetch: fetcher{http: httpClient, attemptTimeout: defaultAttemptTimeout, service: "天气服务（Open-Meteo）"}}
}

// FindPlaces 按地名找坐标：返回地名库里最多 limit 个候选（最相关的在前），一个都没有时返回空。
func (c *OpenMeteoClient) FindPlaces(ctx context.Context, place PlaceQuery, limit int) ([]Place, error) {
	query := url.Values{"name": {place.Name}, "count": {strconv.Itoa(limit)}, "format": {"json"}}
	if place.Language != "" { // 不设 = 地名库默认的语言
		query.Set("language", place.Language)
	}
	if place.CountryCode != "" {
		query.Set("countryCode", strings.ToUpper(place.CountryCode))
	}
	var response struct {
		Results []struct {
			Name        string  `json:"name"`
			Latitude    float64 `json:"latitude"`
			Longitude   float64 `json:"longitude"`
			Country     string  `json:"country"`
			CountryCode string  `json:"country_code"`
			Admin1      string  `json:"admin1"`
		} `json:"results"`
	}
	address, err := endpoint.Join(c.geocodingURL, "", query)
	if err != nil {
		return nil, err
	}
	if err := c.fetch.getJSON(ctx, address, &response); err != nil {
		return nil, err
	}
	places := make([]Place, 0, len(response.Results))
	for _, found := range response.Results {
		places = append(places, Place{Name: found.Name, Latitude: found.Latitude, Longitude: found.Longitude,
			Country: found.Country, Region: found.Admin1, CountryCode: strings.ToUpper(found.CountryCode)})
	}
	return places, nil
}

// errNoCurrentWeather 表示回复里现在的天气各项都是空的（服务那边缺数据）：当作没查到，让上层改用备用来源。
var errNoCurrentWeather = errors.New("Open-Meteo 的回复里没有现在的天气（各项都是空的）")

// ForecastFor 实现 Forecaster：现在的天气和未来 days 天（含今天）的预报，时间按当地时区。
// 回复里是 null 的项不输出（不当成 0 报给模型）；某一天各项都是 null 就不列这一天；现在的天气各项都是 null 时报错。
func (c *OpenMeteoClient) ForecastFor(ctx context.Context, place Place, days int) (Forecast, error) {
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
			Time        string   `json:"time"`
			Temperature *float64 `json:"temperature_2m"`
			FeelsLike   *float64 `json:"apparent_temperature"`
			Humidity    *float64 `json:"relative_humidity_2m"`
			WeatherCode *int     `json:"weather_code"`
			WindSpeed   *float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			Time        []string   `json:"time"`
			WeatherCode []*int     `json:"weather_code"`
			High        []*float64 `json:"temperature_2m_max"`
			Low         []*float64 `json:"temperature_2m_min"`
			RainChance  []*float64 `json:"precipitation_probability_max"`
			RainSum     []*float64 `json:"precipitation_sum"`
		} `json:"daily"`
	}
	address, err := endpoint.Join(c.forecastURL, "", query)
	if err != nil {
		return Forecast{}, err
	}
	if err := c.fetch.getJSON(ctx, address, &response); err != nil {
		return Forecast{}, err
	}
	now := response.Current
	if now.Temperature == nil && now.FeelsLike == nil && now.Humidity == nil && now.WeatherCode == nil && now.WindSpeed == nil {
		return Forecast{}, errNoCurrentWeather
	}
	forecast := Forecast{Place: place, Source: "Open-Meteo", Current: Current{Time: now.Time, Weather: describeOptionalWMOCode(now.WeatherCode),
		Temperature: now.Temperature, FeelsLike: now.FeelsLike, Humidity: now.Humidity, WindSpeed: now.WindSpeed}}
	daily := response.Daily
	for i, date := range daily.Time {
		day := DayCast{Date: date, Weather: describeOptionalWMOCode(dailyValue(daily.WeatherCode, i)),
			High: dailyValue(daily.High, i), Low: dailyValue(daily.Low, i),
			RainChancePercent: dailyValue(daily.RainChance, i), RainMillimeters: dailyValue(daily.RainSum, i)}
		if day.Weather == "" && day.High == nil && day.Low == nil && day.RainChancePercent == nil && day.RainMillimeters == nil {
			continue // 这一天什么都没给
		}
		forecast.Days = append(forecast.Days, day)
	}
	return forecast, nil
}

// dailyValue 取逐日数组里第 i 天的值；数组不够长或这一天是 null 时为 nil。
func dailyValue[T any](values []*T, i int) *T {
	if i < len(values) {
		return values[i]
	}
	return nil
}

// describeOptionalWMOCode 是天气代码的文字；没给代码时为空（不输出）。
func describeOptionalWMOCode(code *int) string {
	if code == nil {
		return ""
	}
	return DescribeWMOCode(*code)
}

// DescribeWMOCode 把 WMO 天气代码翻成中文。
func DescribeWMOCode(code int) string {
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
