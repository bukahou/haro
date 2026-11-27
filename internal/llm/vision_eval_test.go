//go:build eval

// 看图小评测：按线上看画面走的路——幕后专家（internal/vision）——对样本里的每张画面问同一句「这是什么？」，
// 跑几组「专家用的模型 × 思考 × 力度」（evalConfigs），记下判断、用时、用量和费用，
// 用来决定：专家用哪个模型、要不要开思考、思考的力度和每次输出的上限。专家不联网（评测只看认得准不准，也不花搜索费）。
//
// 样本不放在这个仓库（家里拍的照片）：HARO_EVAL_DIR 指向样本目录，里面有
//   - samples.tsv：每行「编号 <Tab> 正确答案」，# 开头的行跳过（旧格式第三列的关键词不再使用，有也忽略）
//   - 每个编号两张画面：<编号>-1280.jpg（宽 1280，和终端默认送给模型的一样）、<编号>-640.jpg（宽 640）
//
// 评审模型（默认 Opus 5.5，HARO_EVAL_JUDGE_MODEL 可改）对照正确答案判三档（◎ 说出读对的具体信息 / ○ 说对是什么 / ✗ 认错），
// 经工具交出结构化的结论；最后仍以人看为准。
// 结果写到样本目录的 results-<时间>.md。跑一次要花钱：先用 HARO_EVAL_DRY_RUN=1 看会发多少请求、大概多少钱。
//
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… go test -tags eval -run VisionEval -v -count=1 -timeout 30m ./internal/llm/
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/vision"
)

// evalConfig 是一组评测配置。单价是官方价（美元 / 百万 token），只用来估算费用。
type evalConfig struct {
	name, model, thinking, effort  string
	width, maxTokens               int
	in, out, cacheRead, cacheWrite float64
}

// 画面宽都是 1280（终端默认送给模型的）。对照是改之前线上的专家（Sonnet，每次输出上限是 HARO_CLOUD_LLM_MAX_TOKENS 的默认 800）；
// 开着思考的组思考也算在输出里，上限放宽到 16000，先量实际要多少。
var evalConfigs = []evalConfig{
	{"Sonnet（改之前线上）", "claude-sonnet-5-5", ThinkingBetweenTools, "low", 1280, 800, 2, 10, 0.10, 2.50},
	{"Haiku 不开思考", "claude-haiku-5-5", ThinkingDisabled, "low", 1280, 800, 0.10, 0.50, 0.01, 0.125},
	{"Haiku 思考 low", "claude-haiku-5-5", ThinkingAdaptive, "low", 1280, 16000, 0.10, 0.50, 0.01, 0.125},
	{"Haiku 思考 medium", "claude-haiku-5-5", ThinkingAdaptive, "medium", 1280, 16000, 0.10, 0.50, 0.01, 0.125},
	{"Haiku 思考 high", "claude-haiku-5-5", ThinkingAdaptive, "high", 1280, 16000, 0.10, 0.50, 0.01, 0.125},
	{"Haiku 思考 xhigh", "claude-haiku-5-5", ThinkingAdaptive, "xhigh", 1280, 16000, 0.10, 0.50, 0.01, 0.125},
}

// 评审（默认 Opus 5.5）的单价（美元 / 百万 token），只用来估算费用。
const (
	evalJudgeIn  = 4.0
	evalJudgeOut = 20.0
)

const evalYenPerUSD = 160

type evalSample struct {
	id, answer string
}

type evalResult struct {
	sample  evalSample
	config  evalConfig
	answer  string
	verdict judgement
	elapsed time.Duration
	usage   brain.ReplyUsage
	yen     float64
	err     error
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
	if dryRun {
		estimate := 0.0
		for _, config := range evalConfigs {
			for range samples {
				imageTokens := float64(config.width * config.width * 9 / 16 / 750)
				output := 300.0 // 不开思考时一次回答约 300 token；开着思考按 2000 估
				if config.thinking == ThinkingAdaptive {
					output = 2000
				}
				estimate += ((imageTokens+1600)*config.in + output*config.out) / 1e6 * evalYenPerUSD // 专家的提示词约 1.6k token
				estimate += (700*evalJudgeIn + 300*evalJudgeOut) / 1e6 * evalYenPerUSD               // 评审每条约 700 输入、300 输出（含思考）
			}
		}
		t.Logf("样本 %d 个 × 配置 %d 组 = %d 次请求（另加同样次数的评审）；估计约 ¥%.0f（另加每组第一次写缓存）", len(samples), len(evalConfigs),
			len(samples)*len(evalConfigs), estimate)
		return
	}
	api := claudeAPIForTest(key, "", nil)
	judgeModel := os.Getenv("HARO_EVAL_JUDGE_MODEL")
	if judgeModel == "" {
		judgeModel = "claude-opus-5-5"
	}
	// adaptive：Opus 5.5 关不掉思考；判断不难，力度 low
	judge := &usageTotal{ChatModel: NewAnthropicChatModel(AnthropicOptions{API: api, Model: judgeModel, MaxTokens: 2000, Effort: "low",
		Thinking: ThinkingAdaptive})}
	catalog, err := agents.Load("") // 评测用程序里带的「看画面」定义（和线上同一份）
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := catalog.Get(agents.Vision)
	rules, err := vision.RulesFrom(definition)
	if err != nil {
		t.Fatal(err)
	}
	// 线上看一次最多等多久（超过就退回把画面交给前台）：评测不按它截断、量实际要多久，报告里数有几次超过
	limit := rules.Timeout
	rules.Timeout = 3 * time.Minute
	var results []evalResult
	for _, config := range evalConfigs {
		model := &usageTotal{ChatModel: NewAnthropicChatModel(AnthropicOptions{API: api, Model: config.model,
			MaxTokens: config.maxTokens, Effort: config.effort, Thinking: config.thinking})}
		expert, err := vision.NewExpert(model, config.name, rules, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		for _, sample := range samples {
			frame, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s-%d.jpg", sample.id, config.width)))
			if err != nil {
				t.Fatal(err)
			}
			result := evalResult{sample: sample, config: config}
			result.answer, result.elapsed, result.usage, result.err = examineAsExpert(expert, model, frame)
			if result.err == nil {
				result.verdict = judgeAnswer(t, judge, sample.answer, result.answer)
			}
			result.yen = (float64(result.usage.PromptTokens-result.usage.CacheReadTokens-result.usage.CacheWriteTokens)*config.in +
				float64(result.usage.CacheReadTokens)*config.cacheRead + float64(result.usage.CacheWriteTokens)*config.cacheWrite +
				float64(result.usage.OutputTokens)*config.out) / 1e6 * evalYenPerUSD
			t.Logf("%s %s@%d：%s %q", sample.id, config.name, config.width, result.verdict.mark(), result.answer)
			results = append(results, result)
		}
	}
	judgeUsage := judge.total()
	judgeUSD := (float64(judgeUsage.PromptTokens)*evalJudgeIn + float64(judgeUsage.OutputTokens)*evalJudgeOut) / 1e6
	report := evalReport(results, limit, judgeModel, judgeUSD)
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
		if len(fields) < 2 {
			t.Fatalf("samples.tsv 这一行至少要有 2 列（编号、正确答案）：%q", line)
		}
		samples = append(samples, evalSample{id: fields[0], answer: fields[1]})
	}
	if len(samples) == 0 {
		t.Fatal("samples.tsv 里没有样本")
	}
	return samples
}

// examineAsExpert 让专家看一次，返回主人会听到的那句（专家没单独交时用它的细节）、用时与这一次的用量
// （专家可能请求模型不止一次，加起来）。不按线上的时限截断（量实际要多久），最多等 3 分钟。
func examineAsExpert(expert *vision.Expert, model *usageTotal, frame []byte) (string, time.Duration, brain.ReplyUsage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model.reset()
	started := time.Now()
	report, err := expert.ExamineFrame(ctx, brain.FrameQuestion{Frame: frame, Request: "这是什么？"})
	answer := report.Spoken
	if answer == "" {
		answer = report.Details
	}
	return answer, time.Since(started), model.total(), err
}

// usageTotal 包一层模型，把专家每次请求的用量加起来（专家只返回文字，不交出用量）。
type usageTotal struct {
	brain.ChatModel
	mu    sync.Mutex
	usage brain.ReplyUsage
}

func (u *usageTotal) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range u.ChatModel.StreamReply(ctx, request) {
			if delta.Usage != nil {
				u.mu.Lock()
				u.usage.PromptTokens += delta.Usage.PromptTokens
				u.usage.OutputTokens += delta.Usage.OutputTokens
				u.usage.CacheReadTokens += delta.Usage.CacheReadTokens
				u.usage.CacheWriteTokens += delta.Usage.CacheWriteTokens
				u.mu.Unlock()
			}
			if !yield(delta, err) {
				return
			}
		}
	}
}

func (u *usageTotal) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.usage = brain.ReplyUsage{}
}

func (u *usageTotal) total() brain.ReplyUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.usage
}

// 评审的三档（和人工复核的 ◎ / ○ / ✗ 一致）。
const (
	visionSpecific   = "specific"
	visionIdentified = "identified"
	visionWrong      = "wrong"
)

var visionMarks = map[string]string{visionSpecific: "◎", visionIdentified: "○", visionWrong: "✗"}

// judgement 是评审模型交出的结论。
type judgement struct {
	Grade  string `json:"grade" jsonschema:"对照正确答案的判断"`
	Reason string `json:"reason" jsonschema:"一句话理由（用中文写）"`
	judged bool   // 评审模型交出了结论（没交出时报告里写「未判」）
}

func (j judgement) mark() string {
	if !j.judged {
		return "未判"
	}
	return visionMarks[j.Grade]
}

// judgeAnswer 请评审模型对照正确答案判断回答，经 submit_judgement 工具交出结构化的结论（不从文字里找词）。
func judgeAnswer(t *testing.T, judge brain.ChatModel, expected, answer string) judgement {
	t.Helper()
	schema, err := jsonschema.For[judgement](nil)
	if err != nil {
		t.Fatal(err)
	}
	schema.Properties["grade"].Enum = []any{visionSpecific, visionIdentified, visionWrong}
	parameters, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	tool := brain.ToolSpec{Name: "submit_judgement", Parameters: parameters,
		Description: "交出判断。specific：说对了是什么，还说出了能拿去搜的具体信息（品牌、型号、包装上的字）并且读对了；" +
			"identified：说对了是什么东西（类别、用途对），没有具体信息、或具体信息只说是「看着像、不确定」；" +
			"wrong：认错了是什么，或者把读错的字、编出来的品牌型号说成确定的。正确答案里写了特别的评判方式时照它。"}
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: "你是评测的评审，只通过 submit_judgement 工具交出判断。"},
		{Role: brain.RoleUser, Text: "正确答案：" + expected + "\n回答：" + answer}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for delta, err := range judge.StreamReply(ctx, brain.ChatRequest{Messages: messages, Tools: []brain.ToolSpec{tool}}) {
		if err != nil {
			t.Logf("评审出错：%v", err)
			return judgement{}
		}
		for _, call := range delta.ToolCalls {
			if call.Name != tool.Name {
				continue
			}
			raw, _ := json.Marshal(call.Arguments)
			var verdict judgement
			if err := json.Unmarshal(raw, &verdict); err == nil && visionMarks[verdict.Grade] != "" {
				verdict.judged = true
				return verdict
			}
		}
	}
	return judgement{}
}

// evalReport 是 Markdown 报告：先每组配置的汇总（三档各几件、用时、超过线上时限 limit 的次数、输出 token、费用），
// 再逐条明细（评审模型的判断只是初判，最后以人看为准）。
func evalReport(results []evalResult, limit time.Duration, judgeModel string, judgeUSD float64) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# 看图小评测（专家）%s\n\n评审 %s（$%.3f）。◎ = 说出读对的具体信息（品牌 / 型号 / 包装上的字），"+
		"○ = 说对是什么，✗ = 认错或把错的说得很肯定。线上看一次最多等 %v。\n\n", time.Now().Format("2006-01-02 15:04"), judgeModel, judgeUSD, limit)
	report.WriteString("| 配置 | ◎ / ○ / ✗ / 未判 | 平均用时 | 最长 | 超过时限 | 输出 token（平均 / 最多） | 费用 |\n|---|---|---|---|---|---|---|\n")
	for _, config := range evalConfigs {
		counts := map[string]int{}
		var count, over, outputs, maxOutput int
		var elapsed, longest time.Duration
		var yen float64
		for _, result := range results {
			if result.config != config {
				continue
			}
			count++
			counts[result.verdict.mark()]++
			elapsed += result.elapsed
			longest = max(longest, result.elapsed)
			if result.elapsed > limit || result.err != nil {
				over++
			}
			outputs += result.usage.OutputTokens
			maxOutput = max(maxOutput, result.usage.OutputTokens)
			yen += result.yen
		}
		if count > 0 {
			fmt.Fprintf(&report, "| %s | %d / %d / %d / %d | %v | %v | %d | %d / %d | ¥%.2f |\n", config.name, counts["◎"], counts["○"],
				counts["✗"], counts["未判"], (elapsed / time.Duration(count)).Round(100*time.Millisecond), longest.Round(100*time.Millisecond),
				over, outputs/count, maxOutput, yen)
		}
	}
	report.WriteString("\n| 编号 | 正确答案 | 配置 | 说给主人听的 | 判 | 用时 | 输入 / 输出 |\n|---|---|---|---|---|---|---|\n")
	for _, result := range results {
		answer := strings.ReplaceAll(result.answer, "\n", " ")
		if result.err != nil {
			answer = "出错：" + result.err.Error()
		}
		fmt.Fprintf(&report, "| %s | %s | %s | %s | %s %s | %v | %d / %d |\n", result.sample.id, result.sample.answer, result.config.name,
			answer, result.verdict.mark(), result.verdict.Reason, result.elapsed.Round(100*time.Millisecond),
			result.usage.PromptTokens, result.usage.OutputTokens)
	}
	return report.String()
}
