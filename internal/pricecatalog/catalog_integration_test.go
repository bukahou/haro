//go:build integration

// 对真的模型价格目录拉一次（默认不跑）：
//
//	go test -tags integration -run LoadLiveCatalog -v ./internal/pricecatalog/
//
// HARO_MODEL_PRICES_URL / HARO_MODEL_PRICES_PROVIDER 不设用默认；要查的模型用 HARO_TEST_PRICED_MODELS（逗号分隔）。
package pricecatalog

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestLoadLiveCatalog(t *testing.T) {
	url, provider := os.Getenv("HARO_MODEL_PRICES_URL"), os.Getenv("HARO_MODEL_PRICES_PROVIDER")
	if url == "" {
		url = "https://models.dev/api.json"
	}
	if provider == "" {
		provider = "anthropic"
	}
	models := strings.Split(os.Getenv("HARO_TEST_PRICED_MODELS"), ",")
	if models[0] == "" {
		models = []string{"claude-haiku-5-5", "claude-sonnet-5-5"}
	}
	catalog, report, err := Load(context.Background(), &http.Client{}, url, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s（%s）拉取于 %s，用的本地副本=%v", url, provider, catalog.FetchedAt.Format("2006-01-02 15:04"), report.FromCache)
	for _, model := range models {
		price, err := catalog.Price(model)
		if err != nil {
			t.Errorf("%s: %v", model, err)
			continue
		}
		t.Logf("%s：输入 %v、输出 %v、缓存读 %v、缓存写 %v（美元 / 百万 token），分档 %+v", model, price.InputUSDPerMillion,
			price.OutputUSDPerMillion, price.CacheReadUSDPerMillion, price.CacheWriteUSDPerMillion, price.Tiers)
	}
}
