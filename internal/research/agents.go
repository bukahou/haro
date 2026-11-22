package research

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bukahou/haro/internal/brain"
)

// 调查用的两个代理（定义在 internal/agents 的 research.md、research_review.md，装配处按定义接好模型交给 NewRunner）：
// 调查（便宜的模型）查资料、交草稿；审查（更强的模型，可选）只核对草稿。两个代理的工具由这里提供（不在会话的工具箱里），
// 工具名、正文里引用的值都只在这里定义。

// Agents 是调查用的代理。
type Agents struct {
	// Investigator 查资料、交草稿：能用 search_web、read_pages、submit_conclusion（允许名单里有的）。
	// MaxSteps 是搜索阶段最多请求模型几轮（R2 的轮数上限）。
	Investigator brain.Agent
	// Reviewer 核对草稿：只用 submit_review 交回，只请求一次（MaxSteps 是 1），Timeout 是审一次最多多久；nil = 不审查。
	Reviewer *brain.Agent
}

// 两个代理的工具名。
const (
	searchWebTool        = "search_web"
	readPagesTool        = "read_pages"
	submitConclusionTool = "submit_conclusion"
	submitReviewTool     = "submit_review"
)

// InvestigationTools 是调查的代理能用的工具名：装配处按它检查定义的允许名单（写错的名字起不来）。
func InvestigationTools() []string {
	return []string{searchWebTool, readPagesTool, submitConclusionTool}
}

// ReviewTools 是审查的代理能用的工具名（同上）。
func ReviewTools() []string { return []string{submitReviewTool} }

// InvestigationPromptValues 是代理「调查」正文里引用的值。工具名只填允许名单（allowed，nil = 全部）里有的：正文提到没交给它的
// 工具时填值报错、服务端起不来，不会让模型去调用一个没给它的工具。maxRounds、limits 是每个任务的上限；reviewed 是有没有审查
// （有才讲复查的做法），以及复查时额外的搜索次数和轮数。
func InvestigationPromptValues(allowed []string, maxRounds int, limits Limits, reviewed bool) map[string]any {
	values := map[string]any{"MaxSearches": limits.MaxSearches, "MaxRounds": maxRounds, "Reviewed": reviewed,
		"RecheckSearches": recheckSearches, "RecheckRounds": recheckRounds}
	addToolNames(values, allowed, map[string]string{"SearchTool": searchWebTool, "ReadTool": readPagesTool,
		"SubmitTool": submitConclusionTool})
	return values
}

// ReviewPromptValues 是代理「调查审查」正文里引用的值（工具名同上，只填允许名单里有的）。
func ReviewPromptValues(allowed []string) map[string]any {
	values := map[string]any{}
	addToolNames(values, allowed, map[string]string{"ReviewTool": submitReviewTool})
	return values
}

func addToolNames(values map[string]any, allowed []string, names map[string]string) {
	for key, tool := range names {
		if allowed == nil || slices.Contains(allowed, tool) {
			values[key] = tool
		}
	}
}

// check 检查两个代理（执行的一方自己守门，不假设装配处查过）：调查要能搜、能交结论；审查要能交回审查、只请求一次、有时限。
// 写了也不会生效的项（重复调用的检查、调查的 timeout）不许写。
func (a Agents) check() error {
	var problems []string
	investigator := a.Investigator
	if investigator.Model == nil || strings.TrimSpace(investigator.Instructions) == "" {
		problems = append(problems, "调查的代理要有模型和系统提示词")
	}
	if !allows(investigator, searchWebTool) || !allows(investigator, submitConclusionTool) {
		problems = append(problems, fmt.Sprintf("调查的代理要能用 %s 搜、用 %s 交结论，现在的工具：%v", searchWebTool,
			submitConclusionTool, investigator.Tools))
	}
	if investigator.MaxSteps < 1 {
		problems = append(problems, fmt.Sprintf("调查的轮数要至少 1，现在是 %d", investigator.MaxSteps))
	}
	if investigator.RepeatLimit != 0 || investigator.Timeout != 0 {
		problems = append(problems, "调查的代理不按重复的调用判断死循环（轮数、搜索次数有上限），时限由 HARO_RESEARCH_TIMEOUT 定："+
			"repeat_limit、timeout 不用写")
	}
	if reviewer := a.Reviewer; reviewer != nil {
		if reviewer.Model == nil || strings.TrimSpace(reviewer.Instructions) == "" {
			problems = append(problems, "审查的代理要有模型和系统提示词")
		}
		if !allows(*reviewer, submitReviewTool) {
			problems = append(problems, fmt.Sprintf("审查的代理要能用 %s 交回，现在的工具：%v", submitReviewTool, reviewer.Tools))
		}
		if reviewer.MaxSteps != 1 {
			problems = append(problems, fmt.Sprintf("审查只请求一次：max_steps 要是 1，现在是 %d", reviewer.MaxSteps))
		}
		if reviewer.Timeout <= 0 {
			problems = append(problems, fmt.Sprintf("审查的代理要有正的 timeout，现在是 %v", reviewer.Timeout))
		}
		if reviewer.RepeatLimit != 0 {
			problems = append(problems, "审查只请求一次，repeat_limit 不用写")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}

// allows 判断代理能不能用这个工具（允许名单为 nil = 都能用）。
func allows(agent brain.Agent, tool string) bool {
	return agent.Tools == nil || slices.Contains(agent.Tools, tool)
}

// offered 是交给代理的工具：specs 里它的允许名单有的（顺序不变）。
func offered(agent brain.Agent, specs ...brain.ToolSpec) []brain.ToolSpec {
	var allowed []brain.ToolSpec
	for _, spec := range specs {
		if allows(agent, spec.Name) {
			allowed = append(allowed, spec)
		}
	}
	return allowed
}
