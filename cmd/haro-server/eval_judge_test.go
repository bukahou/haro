//go:build eval

// 几个评测（前台能力、后台调查）共用的：评审经工具交出三档判断、评审的模型（记下花了多少）、读环境变量的小工具。
package main

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/pricecatalog"
	"github.com/bukahou/haro/internal/spending"
)

// 判分的三档（评审经工具交出）。
const (
	gradeCorrect = "correct"
	gradePartial = "partial"
	gradeWrong   = "wrong"
)

// evalVerdict 是评审的一次判断；judged = false 表示评审出错或没用工具交回。
type evalVerdict struct {
	Grade  string `json:"grade" jsonschema:"对照参考答案的判断"`
	Reason string `json:"reason" jsonschema:"一两句理由（用中文写）：对在哪、错在哪"`
	judged bool
}

func (v evalVerdict) score() float64 {
	switch {
	case !v.judged:
		return 0
	case v.Grade == gradeCorrect:
		return 1
	case v.Grade == gradePartial:
		return 0.5
	}
	return 0
}

func (v evalVerdict) mark() string {
	if !v.judged {
		return "未判"
	}
	return map[string]string{gradeCorrect: "✓ 对", gradePartial: "◐ 部分对", gradeWrong: "✗ 错"}[v.Grade]
}

// askJudge 请评审判一次：role 是评审的身份说明（系统提示词），material 是交给它看的全部内容，
// grades 是 submit_grade 的说明（三档各指什么）。出错、没用工具交回时返回 judged = false 的判断（照实记成「未判」）。
func askJudge(t *testing.T, judge brain.ChatModel, role, material, grades string) evalVerdict {
	t.Helper()
	schema, err := jsonschema.For[evalVerdict](nil)
	if err != nil {
		t.Fatal(err)
	}
	schema.Properties["grade"].Enum = []any{gradeCorrect, gradePartial, gradeWrong}
	parameters, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	tool := brain.ToolSpec{Name: "submit_grade", Parameters: parameters, Description: grades}
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: role + "只通过 submit_grade 工具交出判断。"},
		{Role: brain.RoleUser, Text: material}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for delta, err := range judge.StreamReply(ctx, brain.ChatRequest{Messages: messages, Tools: []brain.ToolSpec{tool}}) {
		if err != nil {
			t.Logf("评审出错：%v", err)
			return evalVerdict{}
		}
		for _, call := range delta.ToolCalls {
			if call.Name != tool.Name {
				continue
			}
			raw, _ := json.Marshal(call.Arguments)
			var verdict evalVerdict
			if json.Unmarshal(raw, &verdict) == nil && slices.Contains([]string{gradeCorrect, gradePartial, gradeWrong}, verdict.Grade) {
				verdict.judged = true
				return verdict
			}
		}
	}
	return evalVerdict{}
}

// evalJudge 是评审的模型（Claude）：记下花了多少美元。
type evalJudge struct {
	brain.ChatModel
	model   string
	pricing spending.Pricing

	mu  sync.Mutex
	usd float64
}

// newEvalJudge 建评审的模型：单价按模型价格目录（汇率记成 1，花费即美元）。thinking 按模型选（Opus 5.5 关不掉思考，用 adaptive）。
func newEvalJudge(t *testing.T, settings config.ServerSettings, key, model, thinking string, maxTokens int) *evalJudge {
	t.Helper()
	prices, _, err := pricecatalog.Load(context.Background(), &http.Client{Timeout: 30 * time.Second}, settings.ModelPricesURL,
		settings.ModelPricesProvider, "")
	if err != nil {
		t.Fatal(err)
	}
	price, err := prices.Price(model)
	if err != nil {
		t.Fatal(err)
	}
	api, err := llm.NewClaudeAPI(key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &evalJudge{ChatModel: llm.NewAnthropicChatModel(llm.AnthropicOptions{API: api, Model: model, MaxTokens: maxTokens,
		Effort: "low", Thinking: thinking, MaxRetries: 2}), model: model,
		pricing: spending.Pricing{USDToYen: 1, Models: map[string]spending.TokenPrice{model: price}}}
}

func (j *evalJudge) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range j.ChatModel.StreamReply(ctx, request) {
			if delta.Usage != nil {
				j.mu.Lock()
				j.usd += j.pricing.LLMYen(j.model, *delta.Usage)
				j.mu.Unlock()
			}
			if !yield(delta, err) {
				return
			}
		}
	}
}

// spent 是到现在评审花了多少美元。
func (j *evalJudge) spent() float64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.usd
}

// evalList 把逗号分隔的环境变量拆成一项项（去掉空白和空项）。
func evalList(raw string) []string {
	var items []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func envFloat(t *testing.T, name string, fallback float64) float64 {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 {
		t.Fatalf("%s 要是正数：%q", name, raw)
	}
	return value
}

// evalOneLine 把文字压成一行，超过 limit 个字截断。
func evalOneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// latestRun 是要判分的回答：HARO_EVAL_RUN 指定的，没指定时用 results 里最新的 run-*.jsonl。
func latestRun(t *testing.T, dir string) string {
	t.Helper()
	if path := os.Getenv("HARO_EVAL_RUN"); path != "" {
		return path
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "results", "run-*.jsonl"))
	if len(matches) == 0 {
		t.Fatal("results 里没有 run-*.jsonl")
	}
	slices.Sort(matches)
	return matches[len(matches)-1]
}
