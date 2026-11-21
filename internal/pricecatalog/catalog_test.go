package pricecatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/spending"
)

// 目录的一小段：照 models.dev 的格式，两家厂商；haiku 有「上下文超过 10 万 token」的一档。
const testCatalog = `{
  "anthropic": {"id": "anthropic", "name": "Anthropic", "models": {
    "claude-haiku-5-5": {"id": "claude-haiku-5-5", "cost": {"input": 0.1, "output": 0.5, "cache_read": 0.01, "cache_write": 0.125,
      "tiers": [{"input": 0.5, "output": 2.5, "cache_read": 0.05, "cache_write": 0.625, "tier": {"type": "context", "size": 100000}}]}},
    "claude-sonnet-5-5": {"id": "claude-sonnet-5-5", "cost": {"input": 2, "output": 10, "cache_read": 0.1, "cache_write": 2.5}},
    "claude-old": {"id": "claude-old", "cost": {"input": 3, "output": 15, "context_over_200k": {"input": 6, "output": 22.5}}},
    "claude-odd": {"id": "claude-odd", "cost": {"input": 1, "output": 5, "tiers": [{"input": 2, "output": 10, "tier": {"type": "time_of_day", "size": 1}}]}},
    "claude-free-preview": {"id": "claude-free-preview"}
  }},
  "openai": {"id": "openai", "models": {"gpt-x": {"id": "gpt-x", "cost": {"input": 1, "output": 4}}}}
}`

func catalogServer(t *testing.T, healthy *atomic.Bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if !healthy.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(testCatalog))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// 拉到了：取出那一家的单价（含分档，按门槛从低到高），存一份到本地。
func TestLoadFetchesAndSaves(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server, _ := catalogServer(t, &healthy)
	cache := filepath.Join(t.TempDir(), "data", "model-prices.json")
	catalog, report, err := Load(context.Background(), server.Client(), server.URL, "anthropic", cache)
	if err != nil || report.FromCache || report.SaveErr != nil || catalog.FetchedAt.IsZero() {
		t.Fatalf("应拉到并存下：%+v %v", report, err)
	}
	haiku, err := catalog.Price("claude-haiku-5-5")
	if err != nil || haiku.InputUSDPerMillion != 0.1 || haiku.OutputUSDPerMillion != 0.5 || haiku.CacheReadUSDPerMillion != 0.01 ||
		haiku.CacheWriteUSDPerMillion != 0.125 || len(haiku.Tiers) != 1 || haiku.Tiers[0].AboveInputTokens != 100000 ||
		haiku.Tiers[0].Price.InputUSDPerMillion != 0.5 || haiku.Tiers[0].Price.CacheWriteUSDPerMillion != 0.625 {
		t.Errorf("haiku 的单价：%+v %v", haiku, err)
	}
	old, err := catalog.Price("claude-old")
	if err != nil || len(old.Tiers) != 1 || old.Tiers[0].AboveInputTokens != 200_000 || old.Tiers[0].Price.InputUSDPerMillion != 6 {
		t.Errorf("老写法的「超过 20 万」也算一档：%+v %v", old, err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("应存一份到本地：%v", err)
	}
}

// 目录里没有、没标价、分档的方式认不出来：报错，不按猜的价格记账。
func TestPriceRefusesWhatItCannotPrice(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server, _ := catalogServer(t, &healthy)
	catalog, _, err := Load(context.Background(), server.Client(), server.URL, "anthropic", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"claude-nope", "claude-free-preview", "gpt-x"} {
		if _, err := catalog.Price(model); !errors.Is(err, ErrUnknownModel) {
			t.Errorf("%s：应报目录里没有：%v", model, err)
		}
	}
	if _, err := catalog.Price("claude-odd"); err == nil {
		t.Error("认不出来的分档应报错")
	}
}

// 拉不到：用上次存的（记下拉取失败的原因）；连存的都没有就报错；厂商换了不用旧的那份。
func TestLoadFallsBackToSavedCopy(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server, requests := catalogServer(t, &healthy)
	cache := filepath.Join(t.TempDir(), "model-prices.json")
	first, _, err := Load(context.Background(), server.Client(), server.URL, "anthropic", cache)
	if err != nil {
		t.Fatal(err)
	}
	healthy.Store(false)
	catalog, report, err := Load(context.Background(), server.Client(), server.URL, "anthropic", cache)
	if err != nil || !report.FromCache || report.FetchErr == nil || requests.Load() != 2 || !catalog.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("拉不到时应用本地存的：%+v %v", report, err)
	}
	if price, err := catalog.Price("claude-sonnet-5-5"); err != nil || price.OutputUSDPerMillion != 10 {
		t.Errorf("本地存的那份照样能查：%+v %v", price, err)
	}
	if _, _, err := Load(context.Background(), server.Client(), server.URL, "openai", cache); err == nil {
		t.Error("本地存的是别的厂商：不该用")
	}
	if _, _, err := Load(context.Background(), server.Client(), server.URL, "anthropic", filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("拉不到又没有存的：应报错")
	}
	if _, _, err := Load(context.Background(), server.Client(), server.URL, "anthropic", ""); err == nil {
		t.Error("拉不到又没设本地存放处：应报错")
	}
}

// 目录里没有这一家：算拉取失败（有存的就用存的）。
func TestLoadRejectsMissingProvider(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server, _ := catalogServer(t, &healthy)
	if _, report, err := Load(context.Background(), server.Client(), server.URL, "nobody", ""); err == nil || report.FetchErr == nil {
		t.Errorf("没有这一家应报错：%+v %v", report, err)
	}
}

// 目录里的分档接进记账：输入超过 10 万 token 的那次整次按高一档算。
func TestTierPricesApplyAboveThreshold(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server, _ := catalogServer(t, &healthy)
	catalog, _, err := Load(context.Background(), server.Client(), server.URL, "anthropic", "")
	if err != nil {
		t.Fatal(err)
	}
	haiku, err := catalog.Price("claude-haiku-5-5")
	if err != nil {
		t.Fatal(err)
	}
	pricing := spending.Pricing{USDToYen: 100, Models: map[string]spending.TokenPrice{"claude-haiku-5-5": haiku}}
	small := pricing.LLMYen("claude-haiku-5-5", brain.ReplyUsage{PromptTokens: 100_000, OutputTokens: 1000})
	large := pricing.LLMYen("claude-haiku-5-5", brain.ReplyUsage{PromptTokens: 100_001, OutputTokens: 1000})
	// 10 万 × 0.1 + 1000 × 0.5 = 10500 微美元 ⇒ 1.05 日元；超过门槛：100001 × 0.5 + 1000 × 2.5 ≈ 5.25 日元
	if diff := small - 1.05; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("门槛以内按平常的价：%v", small)
	}
	if diff := large - 5.25005; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("超过门槛整次按高一档：%v", large)
	}
}
