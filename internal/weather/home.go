package weather

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// HomeLocator 给出「家」在哪（查天气不说地点时用）。
type HomeLocator interface {
	Home(ctx context.Context) (Place, error)
	// Known 返回现在已经知道的位置（不发网络请求）：byIP = 按网络位置推定的（城市级，可能不准）；还不知道时 ok = false。
	Known() (place Place, byIP, ok bool)
}

// FixedHome 是配置里写死的坐标（名字可以没有）。
type FixedHome struct{ Place Place }

func (h FixedHome) Home(context.Context) (Place, error) { return h.Place, nil }

// Known 实现 HomeLocator。
func (h FixedHome) Known() (Place, bool, bool) { return h.Place, false, true }

// IPHome 按家里的公网 IP 推定位置。精度大约是城市级：
// 运营商有时把 IP 登记在别的城市的机房，可能偏到别的城市 ⇒ 启动时把推定结果写进日志，偏了就改用 HARO_WEATHER_HOME 填坐标。
// 结果缓存一天（家里的 IP 很少变），定位服务连不上时沿用上次的结果。
type IPHome struct {
	url  string
	name string // 配置里给的名字；空 = 用定位出来的城市名
	http *http.Client

	locating singleflight.Group // 同时要定位的只发一次请求，请求不在锁里发：Known 每轮都调用，不能被它卡住
	mu       sync.Mutex
	cached   Place
	fetched  time.Time
}

const ipHomeTTL = 24 * time.Hour

// NewIPHome 创建按 IP 定位的「家」；url 是定位服务（默认 ipinfo.io，HTTPS、不用密钥）。
func NewIPHome(url, name string, httpClient *http.Client) *IPHome {
	return &IPHome{url: url, name: name, http: httpClient}
}

// Cached 返回已经定位好的位置（不发网络请求）；还没定位过时 ok=false。
func (h *IPHome) Known() (Place, bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cached, true, !h.fetched.IsZero()
}

func (h *IPHome) Home(ctx context.Context) (Place, error) {
	h.mu.Lock()
	cached, fetched := h.cached, h.fetched
	h.mu.Unlock()
	if !fetched.IsZero() && time.Since(fetched) < ipHomeTTL {
		return cached, nil
	}
	place, err := sharedRequest(ctx, &h.locating, "home", func(ctx context.Context) (Place, error) {
		place, err := h.locate(ctx)
		if err == nil {
			h.mu.Lock()
			h.cached, h.fetched = place, time.Now()
			h.mu.Unlock()
		}
		return place, err
	})
	if err != nil {
		if !fetched.IsZero() {
			return cached, nil // 定位服务暂时连不上：沿用上次的
		}
		return Place{}, fmt.Errorf("按 IP 定位家的位置失败: %w", err)
	}
	return place, nil
}

func (h *IPHome) locate(ctx context.Context) (Place, error) {
	var response struct {
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Loc     string `json:"loc"` // "纬度,经度"
	}
	locator := fetcher{http: h.http, attemptTimeout: defaultAttemptTimeout, service: "定位服务"}
	if err := locator.getJSON(ctx, h.url, &response); err != nil {
		return Place{}, err
	}
	parts := strings.Split(response.Loc, ",")
	if len(parts) != 2 {
		return Place{}, fmt.Errorf("定位服务没给出坐标：%q", response.Loc)
	}
	latitude, errLat := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	longitude, errLon := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errLat != nil || errLon != nil {
		return Place{}, fmt.Errorf("定位服务的坐标看不懂：%q", response.Loc)
	}
	name := h.name
	if name == "" {
		name = response.City
	}
	return Place{Name: name, Latitude: latitude, Longitude: longitude, Country: response.Country, Region: response.Region}, nil
}
