//go:build eval

// 看图小评测：对样本里的每张画面问同一句「这是什么？」，跑几组「模型 × 思考 × 画面宽度」，记下回答、首字时间、用量和费用，
// 用来决定：带画面的请求交给哪个模型、送给模型的画面多大、要不要开思考。
//
// 样本不放在这个仓库（家里拍的照片）：HARO_EVAL_DIR 指向样本目录，里面有
//   - samples.tsv：每行「编号 <Tab> 正确答案 <Tab> 判对用的关键词（逗号分隔，回答里出现任意一个算对）」，# 开头的行跳过
//   - 每个编号两张画面：<编号>-1280.jpg（宽 1280）、<编号>-640.jpg（宽 640，和终端送给模型的一样）
//
// 结果写到样本目录的 results-<时间>.md。跑一次要花钱：先用 HARO_EVAL_DRY_RUN=1 看会发多少请求、大概多少钱。
//
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… go test -tags eval -run VisionEval -v -count=1 -timeout 30m ./internal/llm/
package llm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// evalConfig 是一组评测配置。单价是官方价（美元 / 百万 token），只用来估算费用。
type evalConfig struct {
	name, model, thinking, effort  string
	width, maxTokens               int
	in, out, cacheRead, cacheWrite float64
}

var evalConfigs = []evalConfig{
	{"Haiku", "claude-haiku-5-5", ThinkingDisabled, "low", 640, 400, 0.10, 0.50, 0.01, 0.125},
	{"Haiku", "claude-haiku-5-5", ThinkingDisabled, "low", 1280, 400, 0.10, 0.50, 0.01, 0.125},
	{"Haiku + 思考", "claude-haiku-5-5", ThinkingAdaptive, "medium", 640, 3000, 0.10, 0.50, 0.01, 0.125},
	{"Haiku + 思考", "claude-haiku-5-5", ThinkingAdaptive, "medium", 1280, 3000, 0.10, 0.50, 0.01, 0.125},
	{"Sonnet", "claude-sonnet-5-5", ThinkingBetweenTools, "low", 640, 400, 2, 10, 0.10, 2.50},
	{"Sonnet", "claude-sonnet-5-5", ThinkingBetweenTools, "low", 1280, 400, 2, 10, 0.10, 2.50},
}

const evalYenPerUSD = 160

type evalSample struct {
	id, answer string
	keywords   []string
}

type evalResult struct {
	sample    evalSample
	config    evalConfig
	answer    string
	correct   bool
	firstText time.Duration
	usage     brain.ReplyUsage
	yen       float64
	err       error
}

func TestVisionEval(t *testing.T) {
	dir := os.Getenv("HARO_EVAL_DIR")
	if dir == "" {
		t.Skip("没设 HARO_EVAL_DIR")
	}
	samples := loadEvalSamples(t, filepath.Join(dir, "samples.tsv"))
	dryRun := os.Getenv("HARO_EVAL_DRY_RUN") != ""
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" && !dryRun {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	// 和正式环境一样的基础提示词（含看画面认东西的规矩）；不给工具，只看图回答
	system := brain.ChatMessage{Role: brain.RoleSystem, Text: brain.DefaultInstructions}
	if dryRun {
		estimate := 0.0
		for _, config := range evalConfigs {
			for range samples {
				imageTokens := float64(config.width * config.width * 9 / 16 / 750)
				estimate += (imageTokens*config.in + 2000*config.cacheRead + 300*config.out) / 1e6 * evalYenPerUSD
			}
		}
		t.Logf("样本 %d 个 × 配置 %d 组 = %d 次请求；估计约 ¥%.0f（另加每组第一次写缓存）", len(samples), len(evalConfigs),
			len(samples)*len(evalConfigs), estimate)
		return
	}
	api := NewClaudeAPI(key, "", nil)
	var results []evalResult
	for _, config := range evalConfigs {
		model := NewAnthropicChatModel(AnthropicOptions{API: api, Model: config.model, MaxTokens: config.maxTokens,
			Effort: config.effort, Thinking: config.thinking})
		for _, sample := range samples {
			frame, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s-%d.jpg", sample.id, config.width)))
			if err != nil {
				t.Fatal(err)
			}
			messages := []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "这是什么？"},
				{Role: brain.RoleUser, Text: "这是摄像头当前画面。", Image: frame}}
			result := evalResult{sample: sample, config: config}
			result.answer, result.firstText, result.usage, result.err = evalRound(model, messages)
			result.correct = result.err == nil && mentionsAny(result.answer, sample.keywords)
			result.yen = (float64(result.usage.PromptTokens-result.usage.CacheReadTokens-result.usage.CacheWriteTokens)*config.in +
				float64(result.usage.CacheReadTokens)*config.cacheRead + float64(result.usage.CacheWriteTokens)*config.cacheWrite +
				float64(result.usage.OutputTokens)*config.out) / 1e6 * evalYenPerUSD
			t.Logf("%s %s@%d：%v %q", sample.id, config.name, config.width, result.correct, result.answer)
			results = append(results, result)
		}
	}
	report := evalReport(results)
	t.Log("\n" + report)
	path := filepath.Join(dir, "results-"+time.Now().Format("20060102-1504")+".md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("结果写到 %s", path)
}

func loadEvalSamples(t *testing.T, path string) []evalSample {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var samples []evalSample
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("samples.tsv 这一行应有 3 列（编号、正确答案、关键词）：%q", line)
		}
		samples = append(samples, evalSample{id: fields[0], answer: fields[1], keywords: strings.Split(fields[2], ",")})
	}
	if len(samples) == 0 {
		t.Fatal("samples.tsv 里没有样本")
	}
	return samples
}

// evalRound 问一次，返回回答、首字时间与用量（不给工具）。
func evalRound(model *AnthropicChatModel, messages []brain.ChatMessage) (string, time.Duration, brain.ReplyUsage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	var firstText time.Duration
	var text strings.Builder
	var usage brain.ReplyUsage
	for delta, err := range model.StreamReply(ctx, messages, nil) {
		if err != nil {
			return text.String(), firstText, usage, err
		}
		if delta.Text != "" && firstText == 0 {
			firstText = time.Since(started)
		}
		text.WriteString(delta.Text)
		if delta.Usage != nil {
			usage = *delta.Usage
		}
	}
	return text.String(), firstText, usage, nil
}

func mentionsAny(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, keyword := range keywords {
		if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" && strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// evalReport 是 Markdown 报告：先每组配置的汇总，再逐条明细（关键词判对只是初判，最后以人看为准）。
func evalReport(results []evalResult) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# 看图小评测 %s\n\n| 配置 | 画面宽 | 关键词判对 | 平均首字 | 费用 |\n|---|---|---|---|---|\n", time.Now().Format("2006-01-02 15:04"))
	for _, config := range evalConfigs {
		var correct, count int
		var firstText time.Duration
		var yen float64
		for _, result := range results {
			if result.config != config {
				continue
			}
			count++
			if result.correct {
				correct++
			}
			firstText += result.firstText
			yen += result.yen
		}
		if count > 0 {
			fmt.Fprintf(&report, "| %s | %d | %d / %d | %v | ¥%.2f |\n", config.name, config.width, correct, count,
				(firstText / time.Duration(count)).Round(10*time.Millisecond), yen)
		}
	}
	report.WriteString("\n| 编号 | 正确答案 | 配置 | 回答 | 判 | 首字 | 输入 / 输出 |\n|---|---|---|---|---|---|---|\n")
	for _, result := range results {
		answer := strings.ReplaceAll(result.answer, "\n", " ")
		if result.err != nil {
			answer = "出错：" + result.err.Error()
		}
		mark := "✗"
		if result.correct {
			mark = "✓"
		}
		fmt.Fprintf(&report, "| %s | %s | %s@%d | %s | %s | %v | %d / %d |\n", result.sample.id, result.sample.answer, result.config.name,
			result.config.width, answer, mark, result.firstText.Round(10*time.Millisecond), result.usage.PromptTokens, result.usage.OutputTokens)
	}
	return report.String()
}
