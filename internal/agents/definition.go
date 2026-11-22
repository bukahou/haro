// Package agents 读代理的定义。一个代理 = 名字、模型档、系统提示词、能用的工具、步数上限、记账用途。
//
// 每个代理一个 markdown 文件：开头两行 --- 之间是 YAML 配置，后面的正文是系统提示词
// （正文可以用 {{.名字}} 引用代码给的值，例如情绪标记的列表、字数上限：这些值只在代码里定义一处）。
// 程序里带一套（builtin/）；部署时可以给一个目录（HARO_AGENTS_DIR），里面同名的文件替换程序里带的那份。
// 目录里只能放程序认识的代理（文件名写错时起不来，免得以为改了、其实没生效）。
//
// 模型只写用哪一档（base / strong），具体的模型 ID、思考写法、等待上限都在配置里，只有一处。
package agents

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/bukahou/haro/internal/frontmatter"
)

// Mode 是代理被调用的方式。
type Mode string

const (
	// ModePrimary 主对话：正文是基础规则，系统提示词由它和性格、主人资料、工具介绍拼成。
	ModePrimary Mode = "primary"
	// ModeHandoff 接手：带着完整的对话历史回答这一轮剩下的部分，它的话直接说给主人；沿用对话的系统提示词，正文留空。
	ModeHandoff Mode = "handoff"
	// ModeSubtask 子任务：不带历史，只带任务说明（和图片），交回一份结果，由调用它的代理转述；正文是它自己的系统提示词。
	ModeSubtask Mode = "subtask"
)

// 模型档。
const (
	TierBase   = "base"   // 云端的模型（HARO_CLOUD_LLM_*；HARO_LLM_PROVIDER=local 时是本地模型）
	TierStrong = "strong" // 更强的模型（HARO_STRONG_LLM_*）
)

// 程序用到的代理（文件名，不含 .md）。
const (
	Front          = "front"           // 前台：主对话
	Strong         = "strong"          // 强模型：主人纠正时（或看画面接手时）接手这一轮
	Vision         = "vision"          // 看画面：幕后专家
	Summary        = "summary"         // 摘要：滚动摘要
	OverBudget     = "over_budget"     // 额度判断：月度上限到了以后判断主人要不要临时提高
	Research       = "research"        // 调查：后台调查查资料、交结论草稿
	ResearchReview = "research_review" // 调查审查：核对调查的结论草稿，交回通过或疑点
)

// known 是程序认识的代理，以及每个该用的调用方式（程序按这个方式调用它）。
var known = map[string]Mode{Front: ModePrimary, Strong: ModeHandoff, Vision: ModeSubtask, Summary: ModeSubtask,
	OverBudget: ModeSubtask, Research: ModeSubtask, ResearchReview: ModeSubtask}

// agentFields 是只有部分代理认的配置项：值为 true 表示这个代理必须写。写在不认的代理上不会生效，起不来比悄悄不生效好。
// 调查的时限、搜索次数由配置定（HARO_RESEARCH_*），不写在定义里。
var agentFields = map[string]map[string]bool{
	Vision:         {"timeout": true, "max_searches": true},
	Summary:        {"timeout": true},
	ResearchReview: {"timeout": true},
}

// Definition 是一个代理的定义。
type Definition struct {
	Name    string   `yaml:"-"`       // 文件名（不含 .md）
	Title   string   `yaml:"title"`   // 日志里的名字
	Mode    Mode     `yaml:"mode"`    // 调用方式
	Model   string   `yaml:"model"`   // 模型档：base / strong
	Purpose string   `yaml:"purpose"` // 记账用途（费用按它分开记）
	Tools   []string `yaml:"tools"`   // 能用的工具（允许名单）；不写 = 调用方提供的全部，写 [] = 不用工具
	// MaxSteps 是一次最多请求模型几次（最后一次不许调用工具，逼它用文字收尾）
	MaxSteps int `yaml:"max_steps"`
	// RepeatLimit、RepeatWindow：同一个工具、同样的参数在最近 RepeatWindow 步里出现第 RepeatLimit 次就不再执行；0 = 不查
	RepeatLimit  int `yaml:"repeat_limit"`
	RepeatWindow int `yaml:"repeat_window"`
	// MaxOutputTokens 是每次最多输出多少 token；0 = 用配置里的 HARO_CLOUD_LLM_MAX_TOKENS
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// TimeoutText 是调用一次最多等多久（例如 25s），解析后放在 Timeout；MaxSearches 是一次最多联网查几次。
	// 这两项只有认它们的代理能写（见 agentFields）
	TimeoutText string        `yaml:"timeout"`
	Timeout     time.Duration `yaml:"-"`
	MaxSearches int           `yaml:"max_searches"`

	Prompt string `yaml:"-"` // 正文：系统提示词的模板
	Source string `yaml:"-"` // 从哪读的（只给日志看）
	// Builtin：这份是程序里带的（没被覆盖目录替换）。读的时候按来源记下，不从 Source 的写法推（覆盖目录也可能叫 builtin）
	Builtin bool `yaml:"-"`
	prompt  *template.Template
}

// RenderPrompt 填好正文里引用的值，得到系统提示词。正文引用了 values 里没有的名字时报错（不静默留空）。
func (d Definition) RenderPrompt(values map[string]any) (string, error) {
	var out bytes.Buffer
	if err := d.prompt.Execute(&out, values); err != nil {
		return "", fmt.Errorf("代理 %s（%s）的正文：%w", d.Name, d.Source, err)
	}
	return out.String(), nil
}

// Catalog 是读进来的代理定义（按名字）。
type Catalog map[string]Definition

// Get 取名字是 name 的代理；没有时报错（Load 已检查程序认识的都在，只有写错名字才会报）。
func (c Catalog) Get(name string) (Definition, error) {
	definition, ok := c[name]
	if !ok {
		return Definition{}, fmt.Errorf("没有叫 %s 的代理", name)
	}
	return definition, nil
}

const builtinDir = "builtin"

//go:embed builtin/*.md
var builtin embed.FS

// Load 读代理定义：先读程序里带的，dir 不为空时用其中的 *.md 替换同名的。
// 每份都要格式正确、字段合法、调用方式和程序用它的方式一致；dir 里有程序不认识的代理时报错。
func Load(dir string) (Catalog, error) {
	catalog := Catalog{}
	entries, err := fs.ReadDir(builtin, builtinDir)
	if err != nil {
		return nil, fmt.Errorf("读取程序里带的代理定义: %w", err)
	}
	for _, entry := range entries {
		source := builtinDir + "/" + entry.Name()
		data, err := fs.ReadFile(builtin, source)
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", source, err)
		}
		if err := catalog.add(entry.Name(), source, data, true); err != nil {
			return nil, err
		}
	}
	if dir != "" {
		if err := catalog.override(dir); err != nil {
			return nil, err
		}
	}
	for name := range known {
		if _, ok := catalog[name]; !ok {
			return nil, fmt.Errorf("缺少代理 %s 的定义", name)
		}
	}
	return catalog, nil
}

// override 用 dir 里的 *.md 替换同名的定义（不进子目录）。
func (c Catalog) override(dir string) error {
	if _, err := os.Stat(dir); err != nil { // 配了目录却读不到：多半是挂载没成，不当作「没有覆盖」
		return fmt.Errorf("代理定义目录: %w", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return fmt.Errorf("列出代理定义目录 %s: %w", dir, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取代理定义: %w", err)
		}
		if err := c.add(filepath.Base(path), path, data, false); err != nil {
			return err
		}
	}
	return nil
}

// add 解析一个文件并记下（同名的替换）；builtin 表示是不是程序里带的那份。
func (c Catalog) add(fileName, source string, data []byte, builtin bool) error {
	name := strings.TrimSuffix(fileName, ".md")
	if _, ok := known[name]; !ok {
		return fmt.Errorf("%s：程序不认识叫 %s 的代理（认识的：%s）", source, name, strings.Join(knownNames(), "、"))
	}
	definition, err := parse(name, source, data)
	if err != nil {
		return err
	}
	definition.Builtin = builtin
	c[name] = definition
	return nil
}

func knownNames() []string {
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// parse 解析一个代理文件：--- 之间的 YAML 是配置（不认识的字段报错），之后是正文。
func parse(name, source string, data []byte) (Definition, error) {
	header, body, err := frontmatter.Split(data)
	if err != nil {
		return Definition{}, fmt.Errorf("%s：%w", source, err)
	}
	var definition Definition
	if err := frontmatter.Decode(header, &definition); err != nil {
		return Definition{}, fmt.Errorf("%s 的配置: %w", source, err)
	}
	definition.Name, definition.Source, definition.Prompt = name, source, strings.TrimSpace(string(body))
	if definition.TimeoutText != "" { // 写错了由 validate 报出来
		definition.Timeout, _ = time.ParseDuration(definition.TimeoutText)
	}
	if err := definition.validate(); err != nil {
		return Definition{}, fmt.Errorf("%s：%w", source, err)
	}
	definition.prompt, err = template.New(name).Option("missingkey=error").Parse(definition.Prompt)
	if err != nil {
		return Definition{}, fmt.Errorf("%s 的正文: %w", source, err)
	}
	return definition, nil
}

// validate 检查各字段的取值（不按格式猜默认值：写错了就起不来）。
func (d Definition) validate() error {
	var problems []string
	if strings.TrimSpace(d.Title) == "" {
		problems = append(problems, "title 不能为空")
	}
	if want := known[d.Name]; d.Mode != want {
		problems = append(problems, fmt.Sprintf("mode 要是 %s（程序按这个方式调用它），现在是 %q", want, d.Mode))
	}
	if d.Model != TierBase && d.Model != TierStrong {
		problems = append(problems, fmt.Sprintf("model 只能是 %s 或 %s，现在是 %q", TierBase, TierStrong, d.Model))
	}
	if strings.TrimSpace(d.Purpose) == "" {
		problems = append(problems, "purpose 不能为空")
	}
	if d.MaxSteps < 1 {
		problems = append(problems, fmt.Sprintf("max_steps 至少是 1，现在是 %d", d.MaxSteps))
	}
	if d.RepeatLimit < 0 || d.RepeatWindow < 0 || d.MaxOutputTokens < 0 {
		problems = append(problems, "repeat_limit、repeat_window、max_output_tokens 不能是负数")
	}
	if d.RepeatLimit > 0 && d.RepeatWindow < d.RepeatLimit {
		problems = append(problems, fmt.Sprintf("repeat_window（%d）要不小于 repeat_limit（%d），否则永远判不出死循环", d.RepeatWindow, d.RepeatLimit))
	}
	for field, set := range map[string]bool{"timeout": d.TimeoutText != "", "max_searches": d.MaxSearches != 0} {
		required, allowed := agentFields[d.Name][field]
		switch {
		case set && !allowed:
			problems = append(problems, fmt.Sprintf("这个代理不认 %s（写了也不会生效）", field))
		case !set && required:
			problems = append(problems, fmt.Sprintf("要写 %s", field))
		}
	}
	if d.TimeoutText != "" && d.Timeout <= 0 {
		problems = append(problems, fmt.Sprintf("timeout 要写成正的时长（例如 25s），现在是 %q", d.TimeoutText))
	}
	if d.MaxSearches < 0 {
		problems = append(problems, fmt.Sprintf("max_searches 不能是负数，现在是 %d", d.MaxSearches))
	}
	seen := map[string]bool{}
	for _, tool := range d.Tools {
		if strings.TrimSpace(tool) == "" || seen[tool] {
			problems = append(problems, fmt.Sprintf("tools 里有空的或重复的名字：%q", tool))
		}
		seen[tool] = true
	}
	switch {
	case d.Mode == ModeHandoff && d.Prompt != "":
		problems = append(problems, "接手的代理沿用对话的系统提示词，正文要留空")
	case d.Mode != ModeHandoff && d.Prompt == "":
		problems = append(problems, "正文（系统提示词）不能为空")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}
