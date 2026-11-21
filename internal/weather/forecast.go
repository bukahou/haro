// Package weather 查天气：Open-Meteo 为主（全球、按坐标、不用密钥），日本气象厅网站的数据做备用（只覆盖日本、按地区）。
// 两家都是简单的公开 HTTP 接口、没有官方 MCP 服务 ⇒ 直接内置调用；只发位置，不发别的。
package weather

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// Forecast 是给模型看的天气。来源没给的项不出现（不写成 0，免得模型当成真的 0 度、0 %）。
type Forecast struct {
	Place   Place     `json:"place"`
	Area    string    `json:"area,omitempty"` // 按地区预报的来源（气象厅）用的预报地区与观测站
	Current Current   `json:"current"`
	Days    []DayCast `json:"days"`
	Source  string    `json:"source"`
	Note    string    `json:"note,omitempty"`
}

// Current 是现在的天气。
type Current struct {
	Time         string   `json:"time,omitempty"`
	Station      string   `json:"station,omitempty"` // 实况来自哪个观测站（气象厅）
	Weather      string   `json:"weather,omitempty"`
	Temperature  *float64 `json:"temperature_c,omitempty"`
	FeelsLike    *float64 `json:"feels_like_c,omitempty"`
	Humidity     *float64 `json:"humidity_percent,omitempty"`
	WindSpeed    *float64 `json:"wind_kmh,omitempty"`
	RainLastHour *float64 `json:"rain_last_hour_mm,omitempty"`
}

// DayCast 是某一天的预报。
type DayCast struct {
	Date              string   `json:"date"`
	Weather           string   `json:"weather,omitempty"`
	High              *float64 `json:"high_c,omitempty"`
	Low               *float64 `json:"low_c,omitempty"`
	RainChancePercent *float64 `json:"rain_chance_percent,omitempty"`
	RainMillimeters   *float64 `json:"rain_mm,omitempty"`
}

// Forecaster 查一个地点现在的天气和未来 days 天（含今天，1–7）的预报。Open-Meteo、气象厅各实现一个。
type Forecaster interface {
	ForecastFor(ctx context.Context, place Place, days int) (Forecast, error)
}

// ErrNotCovered 表示这个来源查不了这个地点（例如气象厅只有日本）。
var ErrNotCovered = errors.New("这个天气来源不覆盖这个地点")

// BackupForecaster 在主力查不到时改用备用；备用也查不到时报主力的错。
type BackupForecaster struct {
	primary, backup         Forecaster
	primaryName, backupName string
	logger                  *slog.Logger
}

// WithBackup 给主力天气来源配一个备用。名字写进日志和给模型的说明。
func WithBackup(primary Forecaster, primaryName string, backup Forecaster, backupName string, logger *slog.Logger) *BackupForecaster {
	return &BackupForecaster{primary: primary, backup: backup, primaryName: primaryName, backupName: backupName, logger: logger}
}

// primaryShare：有时限时主力最多用剩下时间的六成，留四成给备用 —— 主力卡住时备用还来得及
// （默认 10 秒的工具时限下备用约 4 秒，够气象厅那条：地址反查、预报、实况）。主力很快就失败时，备用拿到的是全部剩下的时间。
const primaryShare = 0.6

// ForecastFor 实现 Forecaster。主人打断（context 取消）时不再去查备用。
func (b *BackupForecaster) ForecastFor(ctx context.Context, place Place, days int) (Forecast, error) {
	primaryCtx, cancel := withShareOfDeadline(ctx, primaryShare)
	forecast, err := b.primary.ForecastFor(primaryCtx, place, days)
	cancel()
	if err == nil || ctx.Err() != nil {
		return forecast, err
	}
	backup, backupErr := b.backup.ForecastFor(ctx, place, days)
	switch {
	case errors.Is(backupErr, ErrNotCovered):
		return Forecast{}, err
	case backupErr != nil:
		return Forecast{}, fmt.Errorf("%w；备用的%s也没查到：%v", err, b.backupName, backupErr)
	}
	b.logger.Warn("天气：主力没查到，改用备用", "primary", b.primaryName, "backup", b.backupName, "place", place.Name, "err", err)
	backup.Note = strings.TrimSpace(fmt.Sprintf("%s这次没查到，用的是备用的%s。%s", b.primaryName, b.backupName, backup.Note))
	return backup, nil
}

// withShareOfDeadline 返回最多用 ctx 剩下时间的 share 的 context；ctx 没有时限时原样（只是可以取消）。
func withShareOfDeadline(ctx context.Context, share float64) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(float64(time.Until(deadline))*share))
}

// clampDays 把天数限制在 1–7。
func clampDays(days int) int {
	return min(max(days, 1), 7)
}

func ptr(value float64) *float64 { return &value }
