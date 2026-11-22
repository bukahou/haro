package main

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/docstest"
	"github.com/bukahou/haro/internal/spending"
)

// fullTooling 按服务端的装配（buildTooling）把工具都打开：记事本、天气、联网搜索、后台调查。不连网（搜索用到时才连）。
func fullTooling(t *testing.T) tooling {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	settings := config.ServerSettings{Tools: true, NotesDB: filepath.Join(t.TempDir(), "notes.db"), NotesListLimit: 200,
		Weather: true, WeatherBackup: config.WeatherBackupJMA, WeatherGeocodingLanguage: "ja",
		WeatherForecastURL: "http://127.0.0.1:9/v1/forecast", WeatherGeocodingURL: "http://127.0.0.1:9/v1/search",
		WeatherJMAURL: "http://127.0.0.1:9/bosai", WeatherReverseGeocoderURL: "http://127.0.0.1:9/reverse",
		TavilyAPIKey: "test", TavilyMCPURL: "http://127.0.0.1:9/mcp/",
		LLMProvider: config.LLMCloud, CloudLLMAPI: config.CloudAPIOpenAI, CloudLLMURL: "http://127.0.0.1:9/v1", CloudLLMModel: "base-model"}
	ledger, err := spending.OpenLedger(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	deps := services{httpClient: &http.Client{}, modelHTTP: &http.Client{}, ledger: ledger,
		guard:   spending.NewGuard(ledger, spending.Limits{}, logger),
		pricing: spending.Pricing{Models: map[string]spending.TokenPrice{"base-model": {Free: true}}}}
	ctx, cancel := context.WithCancel(context.Background())
	var shutdown shutdownSteps
	shutdown.add("用量账本", ledger.Close)
	t.Cleanup(func() {
		cancel()
		shutdown.run(logger)
	})
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	kit, err := buildTooling(ctx, settings, catalog, deps, logger, &shutdown)
	if err != nil {
		t.Fatal(err)
	}
	return kit
}

// architecture.md 的工具表（模型能触发哪些动作）和代码对得上：工具箱里每个工具都写在表里，要不要过确认闸和工具的标注一致；
// 表里的每个名字都是代码里有的（工具箱或会话自己提供的；会话工具的标注由 brain 的测试核对）。
func TestToolTableInDocsMatchesCode(t *testing.T) {
	table := docstest.ToolTable(t, filepath.Join("..", "..", "docs", "architecture.md"))
	kit := fullTooling(t)
	specs := kit.toolbox.Specs()
	if len(specs) == 0 {
		t.Fatal("工具箱里一个工具都没有：检查装置没生效")
	}
	for _, spec := range specs {
		documented, listed := table[spec.Name]
		switch {
		case !listed:
			t.Errorf("工具 %s 没写进 architecture.md 的工具表", spec.Name)
		case documented != spec.RequiresConfirmation:
			t.Errorf("architecture.md 的工具表里 %s 过不过确认闸写的是 %v，代码里是 %v", spec.Name, documented, spec.RequiresConfirmation)
		}
	}
	if unknown := brain.UnknownTools(slices.Sorted(maps.Keys(table)), kit.toolbox); len(unknown) > 0 {
		t.Errorf("architecture.md 的工具表里有代码里没有的工具：%v", unknown)
	}
}
