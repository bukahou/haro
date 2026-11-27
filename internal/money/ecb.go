package money

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// 欧洲央行的参考汇率：每个工作日下午更新一次（以欧元为基准，公开、不用密钥）。
const (
	ecbFetchTimeout = 20 * time.Second
	ecbMaxBytes     = 1 << 20 // 每天那份只有几 KB，超了按坏数据处理
	ecbRefreshEvery = 6 * time.Hour
	ecbBase         = "EUR"
)

// RateTable 是某一天的汇率：1 欧元换多少各国货币（含 EUR = 1）。
type RateTable struct {
	Date   time.Time          `json:"date"`
	PerEUR map[string]float64 `json:"per_eur"`
}

// convert 把 micros 从 from 换成 to。
func (t RateTable) convert(micros int64, from, to string) (int64, error) {
	if from == to {
		return micros, nil
	}
	fromRate, toRate := t.PerEUR[from], t.PerEUR[to]
	if fromRate <= 0 || toRate <= 0 {
		return 0, fmt.Errorf("%w：%s → %s（%s 的参考汇率）", ErrNoRate, from, to, t.Date.Format(time.DateOnly))
	}
	return int64(math.Round(float64(micros) / fromRate * toRate)), nil
}

// ECBRates 是欧洲央行每天公布的参考汇率（实现 Converter）。启动时 Load 一次，之后 RunRefresh 定时重拉；
// 拉不到时用上次拉到的（存在 cachePath）。
type ECBRates struct {
	url       string
	cachePath string // 空 = 不存
	client    *http.Client
	logger    *slog.Logger

	mu      sync.RWMutex
	current RateTable
}

// NewECBRates 准备汇率（这时不拉）。
func NewECBRates(url, cachePath string, client *http.Client, logger *slog.Logger) *ECBRates {
	return &ECBRates{url: url, cachePath: cachePath, client: client, logger: logger}
}

// Load 启动时拉一次：拉到就存一份到 cachePath；拉不到用上次存的；都没有就报错——不按猜的汇率判断费用上限。
func (r *ECBRates) Load(ctx context.Context) error {
	table, err := r.fetch(ctx)
	if err == nil {
		r.set(table)
		r.save(table)
		return nil
	}
	if r.cachePath == "" {
		return fmt.Errorf("拉取参考汇率（%s）失败，也没有设本地存的那份: %w", r.url, err)
	}
	cached, cacheErr := loadRateTable(r.cachePath)
	if cacheErr != nil {
		return fmt.Errorf("拉取参考汇率（%s）失败: %w；本地存的那份也用不了: %w", r.url, err, cacheErr)
	}
	r.set(cached)
	r.logger.Warn("参考汇率拉不到：用上次存的那份", "err", err, "date", cached.Date.Format(time.DateOnly), "cache", r.cachePath)
	return nil
}

// RunRefresh 每隔一段时间重拉一次（拉不到就留着现在这份、写日志），随 ctx 结束。
func (r *ECBRates) RunRefresh(ctx context.Context) {
	ticker := time.NewTicker(ecbRefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			table, err := r.fetch(ctx)
			if err != nil {
				r.logger.Warn("参考汇率重拉失败：接着用现在这份", "err", err, "date", r.Table().Date.Format(time.DateOnly))
				continue
			}
			r.set(table)
			r.save(table)
		}
	}
}

// Table 是现在用的那份。
func (r *ECBRates) Table() RateTable {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Convert 实现 Converter。
func (r *ECBRates) Convert(sum Sum, to string) (Total, error) {
	table := r.Table()
	total := Total{Amount: Amount{Currency: to}}
	for _, code := range sum.Currencies() {
		micros := sum[code]
		if micros == 0 {
			continue
		}
		converted, err := table.convert(micros, code, to)
		if err != nil {
			return Total{}, err
		}
		total.Amount.Micros += converted
		if code != to {
			total.Converted = append(total.Converted, code)
			total.RateDate = table.Date
		}
	}
	return total, nil
}

func (r *ECBRates) set(table RateTable) {
	r.mu.Lock()
	r.current = table
	r.mu.Unlock()
	r.logger.Info("参考汇率（欧洲央行）", "date", table.Date.Format(time.DateOnly), "currencies", len(table.PerEUR))
}

func (r *ECBRates) save(table RateTable) {
	if r.cachePath == "" {
		return
	}
	if err := saveRateTable(r.cachePath, table); err != nil {
		r.logger.Warn("参考汇率拉到了，但没存到本地（下次拉不到时就没有可退的）", "err", err, "cache", r.cachePath)
	}
}

// ecbEnvelope 是欧洲央行那份 XML 里用得到的部分：Cube（日期）→ Cube（币种、汇率）。
type ecbEnvelope struct {
	Cube struct {
		Days []ecbDay `xml:"Cube"`
	} `xml:"Cube"`
}

type ecbDay struct {
	Time  string    `xml:"time,attr"`
	Rates []ecbRate `xml:"Cube"`
}

type ecbRate struct {
	Currency string  `xml:"currency,attr"`
	Rate     float64 `xml:"rate,attr"`
}

func (r *ECBRates) fetch(ctx context.Context) (RateTable, error) {
	ctx, cancel := context.WithTimeout(ctx, ecbFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return RateTable{}, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return RateTable{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return RateTable{}, fmt.Errorf("参考汇率返回 %d", response.StatusCode)
	}
	return parseECB(io.LimitReader(response.Body, ecbMaxBytes))
}

// parseECB 读欧洲央行的每日参考汇率（最新的一天）。认不出的格式报错，不把半份数据当汇率用。
func parseECB(body io.Reader) (RateTable, error) {
	var envelope ecbEnvelope
	if err := xml.NewDecoder(body).Decode(&envelope); err != nil {
		return RateTable{}, fmt.Errorf("参考汇率的格式认不出: %w", err)
	}
	days := envelope.Cube.Days
	if len(days) == 0 {
		return RateTable{}, errors.New("参考汇率里没有日期")
	}
	day := slices.MaxFunc(days, func(a, b ecbDay) int { return strings.Compare(a.Time, b.Time) })
	date, err := time.Parse(time.DateOnly, day.Time)
	if err != nil {
		return RateTable{}, fmt.Errorf("参考汇率的日期认不出 %q: %w", day.Time, err)
	}
	table := RateTable{Date: date, PerEUR: map[string]float64{ecbBase: 1}}
	for _, rate := range day.Rates {
		code, err := ParseCurrency(rate.Currency)
		if err != nil || rate.Rate <= 0 {
			return RateTable{}, fmt.Errorf("参考汇率里有认不出的一项（%q %v）", rate.Currency, rate.Rate)
		}
		table.PerEUR[code] = rate.Rate
	}
	if len(table.PerEUR) == 1 {
		return RateTable{}, errors.New("参考汇率里没有币种")
	}
	return table, nil
}

func saveRateTable(path string, table RateTable) error {
	data, err := json.Marshal(table)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp := path + ".tmp" // 先写临时文件再改名：写到一半断电也不会留下半份
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func loadRateTable(path string) (RateTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RateTable{}, err
	}
	var table RateTable
	if err := json.Unmarshal(data, &table); err != nil {
		return RateTable{}, fmt.Errorf("存的那份格式不对: %w", err)
	}
	if table.Date.IsZero() || len(table.PerEUR) < 2 {
		return RateTable{}, errors.New("存的那份是空的")
	}
	return table, nil
}
