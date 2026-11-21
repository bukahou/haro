package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// Agent 是装配好的代理：定义里的规则（internal/agents 从文件读）加上接好的模型，由服务端组装。
// 交出去有两种方式，都经 EnterAgent：
//   - 接手（Handover）：带着完整的对话历史，这一轮剩下的部分由它回答，它的话直接说给主人；
//   - 子任务（Subtask）：不带历史，只带任务说明（和图片），交回一份结果，由调用它的代理转述（看画面、摘要）。
type Agent struct {
	Name         string    // 日志里的名字（定义的 title）
	Model        ChatModel // 已按它的记账用途接好（记账、额度、连不上时的熔断）
	Instructions string    // 系统提示词正文（已填好值）；接手的代理为空：沿用对话的系统提示词
	Tools        []string  // 能用的工具（允许名单）；nil = 会话提供的全部
	MaxSteps     int       // 一句话里最多请求模型几次（最后一次不许调用工具）；不到 1 按 1
	// RepeatLimit、RepeatWindow：同一个工具、同样的参数在最近 RepeatWindow 步里出现第 RepeatLimit 次算死循环；0 = 不查
	RepeatLimit  int
	RepeatWindow int
	// Timeout 是调用一次最多等多久（子任务用）；0 = 不另设期限
	Timeout time.Duration
}

// Handoff 是「接手」：哪些情况把这一轮剩下的部分交给 Agent 回答。
type Handoff struct {
	Agent *Agent // nil = 没有能接手的代理
	// OnCorrection：提供 ask_stronger_model，主人纠正、或明说要想清楚时由前台交出去
	OnCorrection bool
	// OnFrame：画面要交给前台模型看时（没有看画面的专家），带画面的请求改由它回答
	OnFrame bool
}

func (h Handoff) corrections() bool { return h.Agent != nil && h.OnCorrection }
func (h Handoff) frames() bool      { return h.Agent != nil && h.OnFrame }

func (a Agent) maxSteps() int { return max(a.MaxSteps, 1) }

// allows 判断代理能不能用这个工具。
func (a Agent) allows(tool string) bool { return a.Tools == nil || slices.Contains(a.Tools, tool) }

// offer 是交给这个代理的工具：会话这一轮提供的里面、它能用的。
func (a Agent) offer(specs []ToolSpec) []ToolSpec {
	if a.Tools == nil || specs == nil {
		return specs
	}
	offered := make([]ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if a.allows(spec.Name) {
			offered = append(offered, spec)
		}
	}
	return offered
}

// UnknownTools 是 tools 里会话和工具箱都不提供的名字（服务端启动时检查代理定义的允许名单，写错了就起不来）。
func UnknownTools(tools []string, toolbox Toolbox) []string {
	known := sessionToolNames()
	if toolbox != nil {
		for _, spec := range toolbox.Specs() {
			known = append(known, spec.Name)
		}
	}
	var unknown []string
	for _, tool := range tools {
		if !slices.Contains(known, tool) {
			unknown = append(unknown, tool)
		}
	}
	return unknown
}

// DelegationMode 是交给别的代理的方式。
type DelegationMode int

const (
	Handover DelegationMode = iota // 接手：换掉代理链的最后一个
	Subtask                        // 子任务：接在代理链后面
)

func (m DelegationMode) String() string {
	if m == Handover {
		return "接手"
	}
	return "子任务"
}

// MaxAgentDepth 是代理链最长几层：前台 → 子任务 → 子任务再开的子任务。子任务里不能无限开子任务。
const MaxAgentDepth = 3

// ErrAgentTooDeep 表示代理嵌套超过了 MaxAgentDepth。
var ErrAgentTooDeep = errors.New("代理嵌套太深")

type agentChainKey struct{}

// AgentChain 是 ctx 里的代理链（谁交给了谁），日志用；最后一个是现在在做事的代理。
func AgentChain(ctx context.Context) []string {
	chain, _ := ctx.Value(agentChainKey{}).([]string)
	return chain
}

// startAgent 是一轮回答的开始：代理链从 name 起。
func startAgent(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, agentChainKey{}, []string{name})
}

// EnterAgent 是交给别的代理的唯一入口（接手、子任务都走这里）：在 ctx 里记下代理链、检查嵌套上限、写日志。
// 后台开始的子任务（ctx 里还没有代理链）从它自己算起。
func EnterAgent(ctx context.Context, logger *slog.Logger, name string, mode DelegationMode) (context.Context, error) {
	chain := slices.Clone(AgentChain(ctx))
	if mode == Handover && len(chain) > 0 {
		chain[len(chain)-1] = name
	} else {
		chain = append(chain, name)
	}
	if len(chain) > MaxAgentDepth {
		return ctx, fmt.Errorf("%w（最多 %d 层）：%s", ErrAgentTooDeep, MaxAgentDepth, strings.Join(chain, " → "))
	}
	logger.Info("交给代理", "agent", name, "mode", mode.String(), "chain", strings.Join(chain, " → "))
	return context.WithValue(ctx, agentChainKey{}, chain), nil
}
