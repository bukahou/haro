package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/bukahou/haro/internal/brain"
)

// ClaudeAPI 是全进程共用的一个 Claude API 客户端（凭证、令牌缓存、HTTP 连接都在里面）。
//
// 认证（v7 D3）：配了 key 就用它；没配时只认 Workload Identity Federation（集群里：ANTHROPIC_FEDERATION_RULE_ID 等环境变量
// + 投射的服务账号令牌，SDK 换成短期令牌并自动续期），不放任何长期密钥。见 claudeCredential。
//
// ⛔ 全进程只能建一个，所有 Claude 模型共用，HTTP 客户端也只能是这一个（2026-11-18 集群实测）：
// SDK 按「客户端 × HTTP 客户端」各建一份令牌缓存、各自去换令牌；集群令牌带 jti，同一张只能换一次 ⇒
// 第二份缓存再去换就被拒（401）——当时主对话能用，摘要和后台调查全挂。
type ClaudeAPI struct {
	client anthropic.Client
}

// CheckCredentials 确认凭证现在能用、连得上 Claude（就绪检查用）：数一次 token，不调用模型、不花钱
// （数 token 是免费的，限流与生成分开算）。集群里用 WIF 时，令牌过期了这一步会顺带换新令牌。
func (a *ClaudeAPI) CheckCredentials(ctx context.Context, model string) error {
	_, err := a.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model:    anthropic.Model(model),
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("ping"))},
	}, option.WithMaxRetries(0))
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("Claude 认证失败（%d）: %w", apiErr.StatusCode, err)
	}
	return err
}

// NewClaudeAPI 创建客户端（不发请求）。apiKey 为空 = 用环境里配的 Workload Identity Federation，没配齐时报错；
// baseURL 为空 = https://api.anthropic.com；httpClient 全进程只用这一个（见类型说明），各用途的超时用请求自己的 context 控制。
func NewClaudeAPI(apiKey, baseURL string, httpClient *http.Client) (*ClaudeAPI, error) {
	credential, err := claudeCredential(apiKey, os.Getenv)
	if err != nil {
		return nil, err
	}
	options := []option.RequestOption{option.WithoutEnvironmentDefaults(), credential}
	if httpClient != nil {
		options = append(options, option.WithHTTPClient(httpClient))
	}
	if baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	return &ClaudeAPI{client: anthropic.NewClient(options...)}, nil
}

// Workload Identity Federation 的环境变量（和 SDK 认的名字一样；这里显式读，见 claudeCredential）。
const (
	envFederationRuleID  = "ANTHROPIC_FEDERATION_RULE_ID"
	envOrganizationID    = "ANTHROPIC_ORGANIZATION_ID"
	envServiceAccountID  = "ANTHROPIC_SERVICE_ACCOUNT_ID"
	envWorkspaceID       = "ANTHROPIC_WORKSPACE_ID"
	envIdentityTokenFile = "ANTHROPIC_IDENTITY_TOKEN_FILE"
	envIdentityToken     = "ANTHROPIC_IDENTITY_TOKEN"
)

// claudeCredential 选凭证：填了 haro 自己的 key（HARO_ANTHROPIC_API_KEY）就用它；没填时只认配齐的 Workload Identity
// Federation（规则、组织、身份令牌文件或令牌）。不走 SDK 的默认凭证链：链里 ANTHROPIC_API_KEY、ANTHROPIC_AUTH_TOKEN、
// 登录档案排在 WIF 前面，开发机上会捡到 Claude Code 的 key，花到别的账上。
func claudeCredential(apiKey string, getenv func(string) string) (option.RequestOption, error) {
	if apiKey != "" {
		return option.WithAPIKey(apiKey), nil
	}
	var token option.IdentityTokenFunc
	if path := getenv(envIdentityTokenFile); path != "" {
		token = option.IdentityTokenFile(path)
	} else if literal := getenv(envIdentityToken); literal != "" {
		token = func(context.Context) (string, error) { return literal, nil }
	}
	rule, organization := getenv(envFederationRuleID), getenv(envOrganizationID)
	var missing []string
	if rule == "" {
		missing = append(missing, envFederationRuleID)
	}
	if organization == "" {
		missing = append(missing, envOrganizationID)
	}
	if token == nil {
		missing = append(missing, envIdentityTokenFile+"（或 "+envIdentityToken+"）")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("没有 Claude 的凭证：开发机填 HARO_ANTHROPIC_API_KEY；集群里配齐 Workload Identity Federation（缺 %s）",
			strings.Join(missing, "、"))
	}
	return option.WithFederationTokenProvider(token, option.FederationOptions{FederationRuleID: rule, OrganizationID: organization,
		ServiceAccountID: getenv(envServiceAccountID), WorkspaceID: getenv(envWorkspaceID)}), nil
}

// CheckClaudeCredentials 检查凭证配齐了没有（启动时查，见 claudeCredential）。
func CheckClaudeCredentials(apiKey string) error {
	_, err := claudeCredential(apiKey, os.Getenv)
	return err
}

// AnthropicChatModel 调用 Claude API 的 Messages 接口（官方 Go SDK，流式；v7 D2）。不同用途（聊天 / 摘要 / 调查）各一个，
// 共用同一个 ClaudeAPI。
//
// 思考默认关掉：工具往来时，思考块（带签名）应原样回传，而 brain.ChatMessage 带不回原始内容块；
// 关掉也最快（聊天 4 秒内要开始回答）。回答花多少力气由 Effort 管。各模型「关掉」的写法不同，见 Thinking。
//
// 提示词缓存两个断点：① 系统提示词末尾（工具定义 + 系统提示词）② 上一轮结束处（之前的对话）。
// 不放在请求末尾：每轮附在主人这句话前面的「现在的情况」（时间、位置……）每轮都变、也不进历史，
// 断点放末尾的话，下一轮从上一句主人的话开始就对不上，之前的对话每轮整段重写（2026-11-18 集群实测：缓存写占花费 8 成）。
type AnthropicChatModel struct {
	api               *ClaudeAPI
	model             string
	maxTokens         int
	effort            anthropic.OutputConfigEffort
	thinking          string
	maxRetries        int
	firstTokenTimeout time.Duration
	requestTimeout    time.Duration
}

// Thinking 的取值。
const (
	// ThinkingDisabled 关掉思考（Haiku 5.5 等）。
	ThinkingDisabled = "disabled"
	// ThinkingBetweenTools 是 Sonnet 5.5 关掉思考的写法：它不接受 disabled（400）；回答前不思考，
	// 只在两次工具调用之间写几句进度说明（以思考块返回；带不回去，丢掉）。
	ThinkingBetweenTools = "between_tools"
	// ThinkingAdaptive 让模型自己决定想多少（思考关不掉的模型用这个）。
	ThinkingAdaptive = "adaptive"
)

// Claude 接受的回答力度（空 = 模型默认）：claudeEfforts 哪种思考写法都行；claudeThinkingEfforts 要开着思考（adaptive），
// 思考关掉时 API 只接受 high 及以下（400）。between_tools 是否接受更高的没有写明，按关掉思考处理。
var (
	claudeEfforts         = []string{"", "low", "medium", "high"}
	claudeThinkingEfforts = []string{"xhigh", "max"}
)

// ValidateClaudeEffort 检查回答力度和它搭配的思考写法（启动时按配置检查，写错了起不来）。
func ValidateClaudeEffort(effort, thinking string) error {
	switch {
	case slices.Contains(claudeEfforts, effort):
		return nil
	case slices.Contains(claudeThinkingEfforts, effort) && thinking == ThinkingAdaptive:
		return nil
	case slices.Contains(claudeThinkingEfforts, effort):
		return fmt.Errorf("%s 要和思考写法 %s 一起用（思考写法是 %q 时只能 low、medium、high）", effort, ThinkingAdaptive, thinking)
	}
	return fmt.Errorf("只能是 low、medium、high，思考写法是 %s 时还可以是 xhigh、max；现在是 %q", ThinkingAdaptive, effort)
}

// ValidateClaudeThinking 检查思考的写法（ThinkingDisabled 等）。
func ValidateClaudeThinking(thinking string) error {
	if !slices.Contains([]string{ThinkingDisabled, ThinkingBetweenTools, ThinkingAdaptive}, thinking) {
		return fmt.Errorf("只能是 %s、%s 或 %s，现在是 %q", ThinkingDisabled, ThinkingBetweenTools, ThinkingAdaptive, thinking)
	}
	return nil
}

// AnthropicOptions 是创建 AnthropicChatModel 的参数。
type AnthropicOptions struct {
	API       *ClaudeAPI // 全进程共用的客户端（必填，见 ClaudeAPI）
	Model     string     // 例如 claude-haiku-5-5
	MaxTokens int        // 每次最多输出多少 token
	Effort    string     // low / medium / high（思考关掉时只能用这三档）；空 = 模型默认
	Thinking  string     // 见 ThinkingDisabled 等；空 = disabled
	// MaxRetries 是 SDK 在 429 / 5xx / 断线时自己重试几次。当主模型时用 0：等不到就早点告诉主人连不上，
	// 不要在这里多等；后台调查不急，可以重试。
	MaxRetries int
	// FirstTokenTimeout 是等 Claude 开始回答（第一个流式事件）最多多久，超时就取消这次请求、按连不上报错；0 = 不限。
	// 不用单独的 HTTP 客户端做「响应头超时」：多一个 HTTP 客户端就多一份令牌缓存（见 ClaudeAPI）。
	FirstTokenTimeout time.Duration
	// RequestTimeout 是一次请求从发出到流结束最多多久；0 = 只受调用方的 context 管（例如后台调查有自己的总时限）。
	// 不放在共用的 HTTP 客户端上：客户端的总超时连读流也算在内，各用途要的长短不同。
	RequestTimeout time.Duration
}

// NewAnthropicChatModel 创建模型（不发请求）。
func NewAnthropicChatModel(options AnthropicOptions) *AnthropicChatModel {
	return &AnthropicChatModel{api: options.API, model: options.Model, maxTokens: options.MaxTokens,
		effort: anthropic.OutputConfigEffort(options.Effort), thinking: options.Thinking, maxRetries: options.MaxRetries,
		firstTokenTimeout: options.FirstTokenTimeout, requestTimeout: options.RequestTimeout}
}

// thinkingConfig 按 Thinking 组装请求里的 thinking。
func (m *AnthropicChatModel) thinkingConfig() anthropic.ThinkingConfigParamUnion {
	switch m.thinking {
	case ThinkingBetweenTools:
		return anthropic.ThinkingConfigParamUnion{OfBetweenTools: &anthropic.ThinkingConfigBetweenToolsParam{}}
	case ThinkingAdaptive:
		return anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
	default:
		return anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
	}
}

// RefusalError 是模型的安全审核拒绝回答（Claude：HTTP 200、stop_reason = refusal；OpenAI 兼容：finish_reason = content_filter）。
// 当作这一次请求失败：有备用模型的（强模型接手时）这一次改用备用；没有的念固定的话。
type RefusalError struct {
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("模型拒绝回答（%s）：%s", e.Category, e.Explanation)
}

// Phrase 见 brain.SpokenError。
func (e *RefusalError) Phrase() (string, map[string]string) { return brain.PhraseRefused, nil }

// StreamReply 流式产出文字；工具调用、用量和停下的原因在流结束时一次性产出（与 OpenAIChatModel 相同）。
// 流要以停下的原因（message_delta）和 message_stop 收尾：连接正常关掉却没收到它们，按流没收尾报错（和断开同一类）。
func (m *AnthropicChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		params, err := m.request(request)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		deadline := newStreamDeadline(ctx, m.requestTimeout, m.firstTokenTimeout)
		defer deadline.close()
		stream := m.api.client.Messages.NewStreaming(deadline.ctx, params, option.WithMaxRetries(m.maxRetries))
		defer stream.Close() // 调用方提前 break 时关掉连接，即中止生成
		var message anthropic.Message
		stopped := false // 收到了 message_stop
		for stream.Next() {
			deadline.started()
			event := stream.Current()
			if err := message.Accumulate(event); err != nil {
				yield(brain.ReplyDelta{Usage: partialUsage(message)}, fmt.Errorf("解析 Claude 的流式响应: %w", err))
				return
			}
			switch event := event.AsAny().(type) {
			case anthropic.MessageStopEvent:
				stopped = true
			case anthropic.ContentBlockDeltaEvent:
				if text, ok := event.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
					if !yield(brain.ReplyDelta{Text: text.Text}, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil { // 出错、被取消：照样交出知道的那部分用量（记账不漏）
			if deadline.noFirstToken() {
				yield(brain.ReplyDelta{Usage: partialUsage(message)},
					markUnavailable(ctx, fmt.Errorf("请求 Claude: %w（等了 %v）", errNoFirstToken, m.firstTokenTimeout)))
				return
			}
			yield(brain.ReplyDelta{Usage: partialUsage(message)}, markUnavailable(ctx, fmt.Errorf("请求 Claude: %w", err)))
			return
		}
		if !stopped || message.StopReason == "" {
			yield(brain.ReplyDelta{Usage: partialUsage(message)}, markUnavailable(ctx, fmt.Errorf("请求 Claude: %w", errStreamUnfinished)))
			return
		}
		usage := usageOf(message.Usage)
		switch message.StopReason {
		case anthropic.StopReasonEndTurn, anthropic.StopReasonToolUse, anthropic.StopReasonStopSequence:
			calls, err := toolCallsOf(message.Content)
			if err != nil {
				yield(brain.ReplyDelta{Usage: usage}, err)
				return
			}
			yield(brain.ReplyDelta{ToolCalls: calls, Usage: usage, Stop: brain.StopCompleted}, nil)
		case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
			// 截断（到了输出上限，或上下文满了）：不交出工具调用（写到一半的参数不完整）
			yield(brain.ReplyDelta{Usage: usage, Stop: brain.StopMaxTokens}, nil)
		case anthropic.StopReasonRefusal:
			// 带上用量：被拒的请求照样计费，记账的装饰器先记下这一笔
			yield(brain.ReplyDelta{Usage: usage}, &RefusalError{Category: string(message.StopDetails.Category),
				Explanation: message.StopDetails.Explanation})
		default: // 认不出的原因（pause_turn 要接着请求才说得完，haro 不用服务端工具、接不上；以后新加的）：不当成说完
			yield(brain.ReplyDelta{Usage: usage}, fmt.Errorf("Claude 停下的原因是 %s: %w", message.StopReason, brain.ErrStopUnknown))
		}
	}
}

// request 组装一次请求。
func (m *AnthropicChatModel) request(request brain.ChatRequest) (anthropic.MessageNewParams, error) {
	messages, tools := request.Messages, request.Tools
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: int64(m.maxTokens),
		Thinking:  m.thinkingConfig(),
	}
	if m.effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: m.effort}
	}
	system, converted := toAnthropicMessages(messages)
	if system != "" {
		// 断点 ①：工具定义 + 系统提示词。对话窗口滑动（最早一轮掉出去）时它们照样命中缓存
		params.System = []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	markPreviousTurnForCache(converted) // 断点 ②
	markToolLoopForCache(converted)     // 断点 ③
	params.Messages = converted
	if len(tools) == 0 {
		// 没有工具的请求，历史里却可能有工具往来（例如后台调查到上限后不再给工具、逼模型用已有资料写结论）：
		// Claude 要求有工具往来的请求带上工具定义，所以按历史里的工具名补上，并声明不许调用
		tools = toolsSeenIn(messages)
	}
	if len(tools) > 0 && (request.ForbidToolCalls || len(request.Tools) == 0) {
		// 工具定义照给（和前几轮一样，断点 ① 之前的缓存照样命中），这一轮不许调用
		params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	}
	if len(tools) > 0 {
		converted, err := toAnthropicTools(tools)
		if err != nil {
			return params, err
		}
		params.Tools = converted
	}
	return params, nil
}

// ——— 消息转换 ———

// toAnthropicMessages 把 brain 的消息转成 Claude 的格式：
//   - 系统消息合并成顶层 system（haro 只在开头放一条）
//   - 工具结果放进紧随其后的用户消息里（Claude 要求如此），看画面的那条用户消息排在工具结果之后、并进同一条
//   - 连续同角色的消息合并成一条
//   - 工具调用 ID 统一重写成 call_N（Claude 只认字母数字、下划线、横线；本地模型 / 别家给的 ID 不一定合格），调用与结果同步改
//   - 配不上对的工具调用 / 结果（Claude 要求每个调用紧跟着它的结果，缺了直接 400）：调用去掉，结果改成普通文字
func toAnthropicMessages(messages []brain.ChatMessage) (string, []anthropic.MessageParam) {
	answered := answeredToolCalls(messages)
	ids := map[string]string{}
	toolUseID := func(original string) string {
		if id, ok := ids[original]; ok {
			return id
		}
		id := "call_" + strconv.Itoa(len(ids))
		ids[original] = id
		return id
	}

	var system []string
	var converted []anthropic.MessageParam
	addUser := func(blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if last := len(converted) - 1; last >= 0 && converted[last].OfUser != nil {
			converted[last].OfUser.Content = append(converted[last].OfUser.Content, blocks...)
			return
		}
		converted = append(converted, anthropic.NewUserMessage(blocks...))
	}
	addAssistant := func(blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if last := len(converted) - 1; last >= 0 && converted[last].OfAssistant != nil {
			converted[last].OfAssistant.Content = append(converted[last].OfAssistant.Content, blocks...)
			return
		}
		converted = append(converted, anthropic.NewAssistantMessage(blocks...))
	}
	for index, msg := range messages {
		switch msg.Role {
		case brain.RoleSystem:
			if strings.TrimSpace(msg.Text) != "" {
				system = append(system, msg.Text)
			}
		case brain.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Text))
			}
			for _, call := range msg.ToolCalls {
				if !answered[index][call.ID] {
					continue
				}
				input := call.Arguments
				if input == nil {
					input = map[string]any{} // Claude 要求 input 是对象
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(toolUseID(call.ID), input, call.Name))
			}
			addAssistant(blocks...)
		case brain.RoleTool:
			if id, ok := ids[msg.ToolCallID]; ok && pairedResult(messages, index) {
				addUser(anthropic.NewToolResultBlock(id, msg.Text, false))
			} else {
				addUser(anthropic.NewTextBlock(fmt.Sprintf("（工具 %s 的结果）%s", msg.ToolName, msg.Text)))
			}
		default: // 用户
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Text))
			}
			if len(msg.Image) > 0 {
				blocks = append(blocks, anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(msg.Image)))
			}
			addUser(blocks...)
		}
	}
	return strings.Join(system, "\n\n"), converted
}

// markPreviousTurnForCache 在「上一轮结束处」打缓存断点：这一轮主人说的话（最后一条不以工具结果开头的用户消息）
// 前面那条消息的最后一块。这一轮的话附着「现在的情况」、每轮都变，放在断点之后；之前的对话下一轮原样再发，从缓存读。
// 第一轮（前面没有对话）不打。
func markPreviousTurnForCache(messages []anthropic.MessageParam) {
	for index := len(messages) - 1; index > 0; index-- {
		user := messages[index].OfUser
		if user == nil || len(user.Content) == 0 || user.Content[0].OfToolResult != nil {
			continue // 助手消息，或这一轮工具往来的结果
		}
		previous := messages[index-1]
		var blocks []anthropic.ContentBlockParamUnion
		switch {
		case previous.OfAssistant != nil:
			blocks = previous.OfAssistant.Content
		case previous.OfUser != nil:
			blocks = previous.OfUser.Content
		}
		if len(blocks) > 0 {
			if cache := blocks[len(blocks)-1].GetCacheControl(); cache != nil {
				*cache = anthropic.NewCacheControlEphemeralParam()
			}
		}
		return
	}
}

// markToolLoopForCache 在一轮里连着调工具时（最后一条消息以工具结果开头）给最后一条消息的最后一块打缓存断点：
// 这一轮的下一步原样带着到这里为止的全部内容，从缓存读，不再每一步都按没缓存的价格重付这一轮已有的工具往来
// （后台调查一个任务就是一轮里十几步）。这一轮的第一步（最后一条是主人的话）不打：一问一答的轮次不多付写缓存的钱。
func markToolLoopForCache(messages []anthropic.MessageParam) {
	if len(messages) == 0 {
		return
	}
	last := messages[len(messages)-1].OfUser
	if last == nil || len(last.Content) == 0 || last.Content[0].OfToolResult == nil {
		return
	}
	if cache := last.Content[len(last.Content)-1].GetCacheControl(); cache != nil {
		*cache = anthropic.NewCacheControlEphemeralParam()
	}
}

// answeredToolCalls 找出每条助手消息里「后面紧跟着结果」的工具调用：answered[助手消息下标][调用 ID]。
// 结果必须紧跟在调用之后（中间只能是别的工具结果）。
func answeredToolCalls(messages []brain.ChatMessage) map[int]map[string]bool {
	answered := map[int]map[string]bool{}
	for index, msg := range messages {
		if msg.Role != brain.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		called := map[string]bool{}
		for _, call := range msg.ToolCalls {
			called[call.ID] = true
		}
		set := map[string]bool{}
		for next := index + 1; next < len(messages) && messages[next].Role == brain.RoleTool; next++ {
			if called[messages[next].ToolCallID] {
				set[messages[next].ToolCallID] = true
			}
		}
		answered[index] = set
	}
	return answered
}

// pairedResult 判断 messages[index]（工具结果）是否紧跟在发起它的那条助手消息之后。
func pairedResult(messages []brain.ChatMessage, index int) bool {
	id := messages[index].ToolCallID
	for previous := index - 1; previous >= 0; previous-- {
		switch messages[previous].Role {
		case brain.RoleTool:
			continue
		case brain.RoleAssistant:
			for _, call := range messages[previous].ToolCalls {
				if call.ID == id {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// toolsSeenIn 列出历史里出现过的工具（只有名字）：这一轮不许调用工具、但历史里有工具往来时补上定义用。
func toolsSeenIn(messages []brain.ChatMessage) []brain.ToolSpec {
	seen := map[string]bool{}
	var specs []brain.ToolSpec
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if !seen[call.Name] {
				seen[call.Name] = true
				specs = append(specs, brain.ToolSpec{Name: call.Name})
			}
		}
	}
	return specs
}

// toAnthropicTools 转换工具定义（参数是 JSON Schema：properties / required 放进对应字段，其余原样带上）。
func toAnthropicTools(tools []brain.ToolSpec) ([]anthropic.ToolUnionParam, error) {
	converted := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		schema := anthropic.ToolInputSchemaParam{}
		if len(tool.Parameters) > 0 {
			var fields map[string]any
			if err := json.Unmarshal(tool.Parameters, &fields); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数定义不是合法 JSON: %w", tool.Name, err)
			}
			for key, value := range fields {
				switch key {
				case "type": // 固定是 object
				case "properties":
					schema.Properties = value
				case "required":
					list, _ := value.([]any)
					for _, name := range list {
						if text, ok := name.(string); ok {
							schema.Required = append(schema.Required, text)
						}
					}
				default:
					if schema.ExtraFields == nil {
						schema.ExtraFields = map[string]any{}
					}
					schema.ExtraFields[key] = value
				}
			}
		}
		if schema.Properties == nil {
			// 没有参数的工具（以及按历史补上的工具）也要有 input_schema：全空时 SDK 会省掉这个字段，Claude 回 400
			schema.Properties = map[string]any{}
		}
		wire := anthropic.ToolParam{Name: tool.Name, InputSchema: schema}
		if tool.Description != "" {
			wire.Description = anthropic.String(tool.Description)
		}
		converted = append(converted, anthropic.ToolUnionParam{OfTool: &wire})
	}
	return converted, nil
}

// ——— 响应转换 ———

// toolCallsOf 取出回答里的工具调用（参数解析成对象）。
func toolCallsOf(content []anthropic.ContentBlockUnion) ([]brain.ToolCall, error) {
	var calls []brain.ToolCall
	for _, block := range content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		arguments := map[string]any{}
		if text := strings.TrimSpace(string(use.Input)); text != "" && text != "null" {
			if err := json.Unmarshal(use.Input, &arguments); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数不是合法 JSON: %w", use.Name, err)
			}
		}
		calls = append(calls, brain.ToolCall{ID: use.ID, Name: use.Name, Arguments: arguments})
	}
	return calls, nil
}

// usageOf 换算用量：PromptTokens 是输入的总数（没命中缓存的 + 缓存读 + 缓存写），三者价格不同，分开记。
// partialUsage 是请求没正常结束时知道的用量：Claude 在流的开头（message_start）就报了输入（含缓存读写），
// 输出数要到结尾（message_delta）才给，所以输出不知道。连开头都没收到时什么都不知道。
func partialUsage(message anthropic.Message) *brain.ReplyUsage {
	if message.ID == "" {
		return &brain.ReplyUsage{Incomplete: true, TokensUnknown: true}
	}
	usage := usageOf(message.Usage)
	usage.OutputTokens, usage.Incomplete = 0, true
	return usage
}

func usageOf(usage anthropic.Usage) *brain.ReplyUsage {
	read, write := int(usage.CacheReadInputTokens), int(usage.CacheCreationInputTokens)
	return &brain.ReplyUsage{PromptTokens: int(usage.InputTokens) + read + write, OutputTokens: int(usage.OutputTokens),
		CacheReadTokens: read, CacheWriteTokens: write}
}
