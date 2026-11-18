package weather

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// JMAArea 是一个地点在气象厅数据里对应的预报地区与观测站。
type JMAArea struct {
	Office, OfficeName   string       // 府县预报区（预报文件按它分）
	Class10, Class10Name string       // 一次细分区域（短期预报的天气、降水概率按它给）
	TempPoint            string       // 短期预报的气温点（观测站代码）
	WeekArea             string       // 周间预报的地区
	WeekTempPoint        string       // 周间预报的气温点
	Stations             []JMAStation // 实况：附近测气温的观测站，近的在前（最近的缺测时用下一个）
}

// JMAStation 是一个观测站。
type JMAStation struct {
	Code, Name string
}

// Describe 是写进日志、交给模型的地区说明。
func (a JMAArea) Describe() string {
	return a.OfficeName + " " + a.Class10Name
}

// 实况最多看附近几个观测站、多远以内（站点平均间隔约 20 公里；一个都不在范围内时只用最近的那个）。
const (
	maxNearbyStations   = 3
	nearbyStationsRange = 40.0 // 公里
)

// jmaTables 是气象厅的地区表、预报地区表、观测站表：第一次用时下载，进程内缓存（很少变）。
type jmaTables struct {
	area struct {
		Offices  map[string]jmaNamed `json:"offices"`
		Class10s map[string]jmaNamed `json:"class10s"`
		Class15s map[string]jmaNamed `json:"class15s"`
		Class20s map[string]jmaNamed `json:"class20s"`
	}
	class20Codes     []string                     // 排好序：按市町村代码找所属的市
	forecastAreas    map[string][]jmaForecastArea // 府县预报区 → 一次细分区域与气温点
	weekAreas        map[string][]jmaWeekArea     // 府县预报区 → 周间预报的地区与气温点
	stations         map[string]jmaStation
	temperatureCodes []string // 测气温的观测站
}

type jmaNamed struct {
	Name   string `json:"name"`
	Parent string `json:"parent"`
}

type jmaForecastArea struct {
	Class10 string   `json:"class10"`
	Amedas  []string `json:"amedas"`
}

type jmaWeekArea struct {
	Srf    string `json:"srf"` // 短期预报的一次细分区域
	Week   string `json:"week"`
	Amedas string `json:"amedas"`
}

type jmaStation struct {
	Name                string
	Latitude, Longitude float64
}

// jmaStationRaw 是观测站表里的一条：坐标是 [度, 分]；elems 第 1 位是 1 表示测气温。
type jmaStationRaw struct {
	Elems  string    `json:"elems"`
	Lat    []float64 `json:"lat"`
	Lon    []float64 `json:"lon"`
	KjName string    `json:"kjName"`
}

// 按坐标缓存的地区最多留多少个（家 + 主人问过的几个地方）。
const maxCachedAreas = 64

// Locate 找出一个地点对应的预报地区与观测站；日本以外、海上报 ErrNotCovered。结果按坐标缓存。
func (j *JMA) Locate(ctx context.Context, place Place) (JMAArea, error) {
	if !inJapan(place) {
		return JMAArea{}, ErrNotCovered
	}
	key := strconv.FormatFloat(place.Latitude, 'f', 3, 64) + "," + strconv.FormatFloat(place.Longitude, 'f', 3, 64)
	j.mu.Lock()
	defer j.mu.Unlock()
	if area, ok := j.areas[key]; ok {
		return area, nil
	}
	tables, err := j.loadTables(ctx)
	if err != nil {
		return JMAArea{}, err
	}
	municipality, err := j.municipality(ctx, place)
	if err != nil {
		return JMAArea{}, err
	}
	area, err := tables.locate(municipality, place)
	if err != nil {
		return JMAArea{}, err
	}
	if len(j.areas) >= maxCachedAreas {
		clear(j.areas)
	}
	j.areas[key] = area
	return area, nil
}

// inJapan 粗筛：日本的经纬度范围以外不用去问（问了也是空）。
func inJapan(place Place) bool {
	return place.Latitude >= 20 && place.Latitude <= 46 && place.Longitude >= 122 && place.Longitude <= 154
}

// loadTables 下载并缓存三张表；调用方持有 j.mu。
func (j *JMA) loadTables(ctx context.Context) (*jmaTables, error) {
	if j.tables != nil {
		return j.tables, nil
	}
	tables := &jmaTables{}
	var stations map[string]jmaStationRaw
	for _, table := range []struct {
		path   string
		target any
	}{
		{"/common/const/area.json", &tables.area},
		{"/forecast/const/forecast_area.json", &tables.forecastAreas},
		{"/forecast/const/week_area.json", &tables.weekAreas},
		{"/amedas/const/amedastable.json", &stations},
	} {
		if err := j.fetch.getJSON(ctx, j.baseURL+table.path, table.target); err != nil {
			return nil, fmt.Errorf("下载气象厅的地区表 %s: %w", table.path, err)
		}
	}
	tables.class20Codes = make([]string, 0, len(tables.area.Class20s))
	for code := range tables.area.Class20s {
		tables.class20Codes = append(tables.class20Codes, code)
	}
	sort.Strings(tables.class20Codes)
	tables.stations = make(map[string]jmaStation, len(stations))
	for code, raw := range stations {
		if len(raw.Lat) != 2 || len(raw.Lon) != 2 {
			continue
		}
		tables.stations[code] = jmaStation{Name: raw.KjName, Latitude: raw.Lat[0] + raw.Lat[1]/60, Longitude: raw.Lon[0] + raw.Lon[1]/60}
		if strings.HasPrefix(raw.Elems, "1") {
			tables.temperatureCodes = append(tables.temperatureCodes, code)
		}
	}
	sort.Strings(tables.temperatureCodes)
	j.tables = tables
	return tables, nil
}

// municipality 用国土地理院的地址反查，找出坐标所在的市町村代码（5 位）；海上、日本以外 ⇒ ErrNotCovered。
func (j *JMA) municipality(ctx context.Context, place Place) (string, error) {
	query := url.Values{"lat": {strconv.FormatFloat(place.Latitude, 'f', 4, 64)}, "lon": {strconv.FormatFloat(place.Longitude, 'f', 4, 64)}}
	var response struct {
		Results struct {
			MuniCd string `json:"muniCd"`
		} `json:"results"`
	}
	if err := j.geocode.getJSON(ctx, j.geocoderURL+"?"+query.Encode(), &response); err != nil {
		return "", err
	}
	code := strings.TrimSpace(response.Results.MuniCd)
	if code == "" || len(code) > 5 {
		return "", ErrNotCovered
	}
	return strings.Repeat("0", 5-len(code)) + code, nil
}

// locate 从市町村代码找预报地区。一个市町村被分成几块、分属不同的一次细分区域时（例如政令市的东部 / 西部），
// 选代表气温点离这个地点最近的那块。
func (t *jmaTables) locate(municipality string, place Place) (JMAArea, error) {
	candidates := t.class20sOf(municipality)
	if len(candidates) == 0 {
		return JMAArea{}, fmt.Errorf("%w：气象厅的地区表里没有市町村 %s", ErrNotCovered, municipality)
	}
	var best JMAArea
	bestDistance := math.Inf(1)
	for _, class20 := range candidates {
		class10 := t.area.Class15s[t.area.Class20s[class20].Parent].Parent
		office := t.area.Class10s[class10].Parent
		if office == "" || best.Class10 == class10 {
			continue
		}
		point, distance := t.nearest(place, t.tempPointsOf(office, class10))
		if best.Office == "" || distance < bestDistance {
			best, bestDistance = JMAArea{Office: office, Class10: class10, TempPoint: point}, distance
		}
	}
	if best.Office == "" {
		return JMAArea{}, fmt.Errorf("%w：气象厅的地区表里市町村 %s 对不上预报地区", ErrNotCovered, municipality)
	}
	best.OfficeName = t.area.Offices[best.Office].Name
	best.Class10Name = t.area.Class10s[best.Class10].Name
	best.WeekArea, best.WeekTempPoint = best.Office, best.TempPoint
	for _, week := range t.weekAreas[best.Office] {
		if week.Srf == best.Class10 {
			best.WeekArea, best.WeekTempPoint = week.Week, week.Amedas
			break
		}
	}
	best.Stations = t.nearby(place)
	return best, nil
}

// nearby 是附近测气温的观测站（近的在前）。
func (t *jmaTables) nearby(place Place) []JMAStation {
	type candidate struct {
		code     string
		distance float64
	}
	candidates := make([]candidate, 0, len(t.temperatureCodes))
	for _, code := range t.temperatureCodes {
		station := t.stations[code]
		candidates = append(candidates, candidate{code, kilometers(place.Latitude, place.Longitude, station.Latitude, station.Longitude)})
	}
	sort.Slice(candidates, func(a, b int) bool { return candidates[a].distance < candidates[b].distance })
	var stations []JMAStation
	for i, c := range candidates {
		if i >= maxNearbyStations || (i > 0 && c.distance > nearbyStationsRange) {
			break
		}
		stations = append(stations, JMAStation{Code: c.code, Name: t.stations[c.code].Name})
	}
	return stations
}

// class20sOf 找市町村对应的二次细分区域：一般是「市町村代码 + 00」；一个市町村被拆成几块时是「+ 01、02…」；
// 政令市的区不单列，归到所属的市 —— 同一都道府县里、代码不超过区代码的最近一个市（区代码紧跟在市代码后面）。
func (t *jmaTables) class20sOf(municipality string) []string {
	if codes := t.withPrefix(municipality); len(codes) > 0 {
		return codes
	}
	i := sort.SearchStrings(t.class20Codes, municipality+"00") - 1
	if i < 0 || t.class20Codes[i][:2] != municipality[:2] {
		return nil
	}
	return t.withPrefix(t.class20Codes[i][:5])
}

func (t *jmaTables) withPrefix(prefix string) []string {
	var codes []string
	for i := sort.SearchStrings(t.class20Codes, prefix); i < len(t.class20Codes) && strings.HasPrefix(t.class20Codes[i], prefix); i++ {
		codes = append(codes, t.class20Codes[i])
	}
	return codes
}

func (t *jmaTables) tempPointsOf(office, class10 string) []string {
	var points []string
	for _, area := range t.forecastAreas[office] {
		if area.Class10 == class10 {
			points = append(points, area.Amedas...)
		}
	}
	return slices.Compact(points)
}

// nearest 返回离地点最近的观测站（只在 codes 里找）；codes 为空时距离为无穷大。
func (t *jmaTables) nearest(place Place, codes []string) (string, float64) {
	best, bestDistance := "", math.Inf(1)
	for _, code := range codes {
		station, ok := t.stations[code]
		if !ok {
			continue
		}
		if distance := kilometers(place.Latitude, place.Longitude, station.Latitude, station.Longitude); distance < bestDistance {
			best, bestDistance = code, distance
		}
	}
	return best, bestDistance
}

// kilometers 是两点间的大致距离（等距圆柱近似，几百公里内够用）。
func kilometers(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0
	x := (lon2 - lon1) * math.Pi / 180 * math.Cos((lat1+lat2)/2*math.Pi/180)
	y := (lat2 - lat1) * math.Pi / 180
	return earthRadius * math.Hypot(x, y)
}
