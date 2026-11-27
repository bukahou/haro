// Package pricecatalog 取模型单价：公共的模型价格目录（models.dev）。服务端启动时拉取，拉到的那一家的价格存一份到本地；
// 拉不到时用上次存的；都没有就报错（费用上限要靠单价折算，不能按 0 记）。
package pricecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/bukahou/haro/internal/spending"
)

const (
	// fetchTimeout：启动时拉目录最多等这么久（目录约 7 MB），拉不到就用本地存的
	fetchTimeout = 20 * time.Second
	// maxCatalogBytes：目录再大也只读这么多（现在约 7 MB），超了按坏数据处理
	maxCatalogBytes = 64 << 20
)

// longCacheWriteFactors：写进 1 小时提示词缓存的输入按普通输入价的几倍算，按厂商（目录里只有 5 分钟缓存的写入价）。
// 来源：厂商的价格说明（Anthropic：1 小时缓存写入 = 基础输入价 × 2）。没写的厂商是 0（没有 1 小时缓存）。
var longCacheWriteFactors = map[string]float64{"anthropic": 2}

// Catalog 是一家厂商（目录里的 provider）的模型单价。
type Catalog struct {
	Provider  string
	FetchedAt time.Time // 这份价格是什么时候从目录拉到的
	models    map[string]wireModel
}

// LoadReport 说明这次用的是哪一份（写日志用）。
type LoadReport struct {
	FromCache bool  // 拉取失败，用的是本地存的那份
	FetchErr  error // 拉取失败的原因
	SaveErr   error // 拉到了，但没存到本地（这次照用，下次拉不到时就没有可退的）
}

// ErrUnknownModel 表示目录里没有这个模型（或它没有标价）。
var ErrUnknownModel = errors.New("模型价格目录里没有这个模型的价格")

// Load 拉取目录里 provider 这一家的价格：拉到了就存一份到 cachePath（空 = 不存）；拉不到时读 cachePath。
func Load(ctx context.Context, client *http.Client, url, provider, cachePath string) (Catalog, LoadReport, error) {
	var report LoadReport
	catalog, err := fetch(ctx, client, url, provider)
	if err == nil {
		if cachePath != "" {
			report.SaveErr = save(cachePath, catalog)
		}
		return catalog, report, nil
	}
	report.FetchErr = err
	if cachePath == "" {
		return Catalog{}, report, fmt.Errorf("拉取模型价格目录（%s）失败，也没有设本地存的那份: %w", url, err)
	}
	cached, cacheErr := load(cachePath, provider)
	if cacheErr != nil {
		return Catalog{}, report, fmt.Errorf("拉取模型价格目录（%s）失败: %w；本地存的那份也用不了: %w", url, err, cacheErr)
	}
	report.FromCache = true
	return cached, report, nil
}

// Price 返回 model 的单价（每百万 token 的美元价，含按上下文分的档）。目录里没有、没标输入输出价、
// 或者分档的方式认不出来时报错：宁可起不来，也不按猜的价格记账。
func (c Catalog) Price(model string) (spending.TokenPrice, error) {
	entry, ok := c.models[model]
	if !ok || entry.Cost == nil || entry.Cost.Input <= 0 || entry.Cost.Output <= 0 {
		return spending.TokenPrice{}, fmt.Errorf("%w：%s（%s）", ErrUnknownModel, model, c.Provider)
	}
	price := entry.Cost.wireRates.tokenPrice()
	for _, tier := range entry.Cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			return spending.TokenPrice{}, fmt.Errorf("%s 的分档价格认不出来（类型 %q）", model, tier.Tier.Type)
		}
		price.Tiers = append(price.Tiers, spending.PriceTier{AboveInputTokens: tier.Tier.Size, Price: tier.wireRates.tokenPrice()})
	}
	if entry.Cost.ContextOver200K != nil { // 老写法：上下文超过 20 万 token 的一档
		price.Tiers = append(price.Tiers, spending.PriceTier{AboveInputTokens: 200_000, Price: entry.Cost.ContextOver200K.tokenPrice()})
	}
	slices.SortFunc(price.Tiers, func(a, b spending.PriceTier) int { return a.AboveInputTokens - b.AboveInputTokens })
	price.LongCacheWriteFactor = longCacheWriteFactors[c.Provider]
	return price, nil
}

// ——— 目录的格式（models.dev 的 api.json：厂商 → 模型 → 单价，每百万 token 的美元价）———

type wireProvider struct {
	Models map[string]wireModel `json:"models"`
}

type wireModel struct {
	Cost *wireCost `json:"cost,omitempty"`
}

type wireRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type wireCost struct {
	wireRates
	Tiers []struct {
		wireRates
		Tier struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier"`
	} `json:"tiers,omitempty"`
	ContextOver200K *wireRates `json:"context_over_200k,omitempty"`
}

func (r wireRates) tokenPrice() spending.TokenPrice {
	return spending.TokenPrice{InputUSDPerMillion: r.Input, OutputUSDPerMillion: r.Output,
		CacheReadUSDPerMillion: r.CacheRead, CacheWriteUSDPerMillion: r.CacheWrite}
}

// cacheFile 是存在本地的那份：只存用到的那一家。
type cacheFile struct {
	Provider  string       `json:"provider"`
	FetchedAt time.Time    `json:"fetched_at"`
	Prices    wireProvider `json:"prices"`
}

// fetch 拉取整个目录，取出 provider 这一家。
func fetch(ctx context.Context, client *http.Client, url, provider string) (Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Catalog{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Catalog{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil {
		return Catalog{}, err
	}
	if len(body) > maxCatalogBytes {
		return Catalog{}, fmt.Errorf("目录超过 %d 字节", maxCatalogBytes)
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(body, &providers); err != nil {
		return Catalog{}, fmt.Errorf("目录不是能读的 JSON: %w", err)
	}
	raw, ok := providers[provider]
	if !ok {
		return Catalog{}, fmt.Errorf("目录里没有厂商 %q", provider)
	}
	var prices wireProvider
	if err := json.Unmarshal(raw, &prices); err != nil {
		return Catalog{}, fmt.Errorf("目录里 %q 这一家的格式看不懂: %w", provider, err)
	}
	return Catalog{Provider: provider, FetchedAt: time.Now().UTC(), models: prices.Models}, nil
}

// save 把这一家的价格存到本地：先写临时文件再改名，写到一半不会留下坏文件。
func save(path string, catalog Catalog) error {
	data, err := json.Marshal(cacheFile{Provider: catalog.Provider, FetchedAt: catalog.FetchedAt, Prices: wireProvider{Models: catalog.models}})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".model-prices-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name()) // 改名成功后它已经不在了
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// load 读本地存的那份；厂商对不上时不用（配置换了厂商）。
func load(path, provider string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, err
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		return Catalog{}, fmt.Errorf("%s 不是能读的价格文件: %w", path, err)
	}
	if cached.Provider != provider {
		return Catalog{}, fmt.Errorf("%s 存的是 %q 的价格，现在要的是 %q", path, cached.Provider, provider)
	}
	return Catalog{Provider: provider, FetchedAt: cached.FetchedAt, models: cached.Prices.Models}, nil
}
