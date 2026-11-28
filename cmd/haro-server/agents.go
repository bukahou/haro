package main

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/backstory"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/chat"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/expenses"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/vision"
)

// 代理的装配：按定义（internal/agents：程序里带的，HARO_AGENTS_DIR 里同名的替换）接好模型交给大脑。
// 模型档（base / strong）换成具体模型、按代理的用途记账，都在这里一处。

// loadAgentDefinitions 读代理定义；用了覆盖目录里的哪份写进日志（程序里带的那份以后更新了，覆盖的不会跟着变）。
func loadAgentDefinitions(settings config.ServerSettings, logger *slog.Logger) (agents.Catalog, error) {
	catalog, err := agents.Load(settings.AgentsDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if definition := catalog[name]; !definition.Builtin {
			logger.Warn("代理定义：用覆盖目录里的这份（程序里带的那份以后更新了也不会跟着变）", "agent", name, "file", definition.Source)
		}
	}
	return catalog, nil
}

// agentAssembly 是装配代理要用的东西（对话的代理、后台调查的代理共用一套）。
type agentAssembly struct {
	settings config.ServerSettings
	catalog  agents.Catalog
	claude   *llm.ClaudeAPI
	client   *http.Client
	pricing  spending.Pricing
	ledger   spending.Ledger
	guard    *spending.Guard
	logger   *slog.Logger
}

// newAgentAssembly 由配置、代理定义和已经连好的外部服务（模型的客户端、费用的账本与额度）得到 agentAssembly。
func newAgentAssembly(settings config.ServerSettings, catalog agents.Catalog, deps services, logger *slog.Logger) agentAssembly {
	return agentAssembly{settings: settings, catalog: catalog, claude: deps.claude, client: deps.modelHTTP, pricing: deps.pricing,
		ledger: deps.ledger, guard: deps.guard, logger: logger}
}

// conversationAgents 是对话用的代理。
type conversationAgents struct {
	front brain.Agent
	// frontText 是打字会话用的前台：同一个模型和工具，说话格式按打字渲染（能用 Markdown、不用情绪标记）
	frontText brain.Agent
	handoff   brain.Handoff     // 主人纠正时（或看画面接手时）接手的强模型；没配时为零值
	expert    brain.FrameExpert // 看画面的专家；没配时为 nil
	summary   *brain.Agent      // 写滚动摘要；没开时为 nil
	title     *brain.Agent      // App 里的对话起标题
	backstory *brain.Agent      // 背景记录池
	receipt   *brain.Agent      // 记账：看小票读成草稿
	// overBudget：月度上限到了以后判断主人要不要临时提高（不受额度管、按天限次数）；不能口头提高时为 nil
	overBudget *brain.Agent
	// breaker：前台和接手的模型共用的「连不上」熔断；刚恢复时由它告诉模型断过多久（每轮的情况说明）
	breaker *llm.UnavailabilityBreaker
}

// build 按配置装配要用的代理：toolbox 用来检查定义里的工具名，expertSearch 是看画面的专家联网查的做法（nil = 不能查）。
func (a agentAssembly) build(toolbox brain.Toolbox, expertSearch vision.Search) (conversationAgents, error) {
	s := a.settings
	built := conversationAgents{breaker: llm.NewUnavailabilityBreaker(unavailableRetry, a.logger)}
	frontDefinition := a.catalog[agents.Front]
	frontModel, _, err := a.metered(frontDefinition)
	if err != nil {
		return built, err
	}
	// 前台外面依次套：连不上时的熔断、按月度额度放行
	front, err := a.agent(frontDefinition, spending.NewBudgetedChatModel(built.breaker.Guard(frontModel), a.guard, a.logger),
		brain.PromptValues(), toolbox)
	if err != nil {
		return built, err
	}
	frontText := front
	if frontText.Instructions, err = frontDefinition.RenderPrompt(brain.PromptValuesFor(brain.MediumText)); err != nil {
		return built, fmt.Errorf("前台（打字）: %w", err)
	}
	if s.ExtraSystemPrompt != "" {
		front.Instructions += "\n\n" + s.ExtraSystemPrompt
		frontText.Instructions += "\n\n" + s.ExtraSystemPrompt
	}
	built.front, built.frontText = front, frontText

	if correction, frames := s.StrongLLMFor(config.StrongForCorrection), s.VisionTakeover(); correction || frames {
		definition := a.catalog[agents.Strong]
		strongModel, model, err := a.metered(definition)
		if err != nil {
			return built, err
		}
		// 它出错或等太久时由前台的模型接手（照常按前台的用途记账）；和前台共用熔断：同一个模型服务
		strongOrFront := llm.NewFallbackChatModel(strongModel, definition.Title, frontModel, frontDefinition.Title, strongCooldown, a.logger)
		strong, err := a.agent(definition, spending.NewBudgetedChatModel(built.breaker.Guard(strongOrFront), a.guard, a.logger), nil, toolbox)
		if err != nil {
			return built, err
		}
		built.handoff = brain.Handoff{Agent: &strong, OnCorrection: correction, OnFrame: frames}
		a.logger.Info("代理：可以交给强模型接手", "agent", strong.Name, "model", model, "on_correction", correction, "on_frame", frames,
			"effort", s.StrongLLMEffort, "thinking", s.StrongLLMThinking)
	}

	if s.VisionExpert() {
		definition := a.catalog[agents.Vision]
		visionModel, model, err := a.metered(definition)
		if err != nil {
			return built, err
		}
		rules, err := vision.RulesFrom(definition)
		if err != nil {
			return built, err
		}
		// 它出错或等太久时，brain 退回把画面直接交给前台模型
		expert, err := vision.NewExpert(spending.NewBudgetedChatModel(visionModel, a.guard, a.logger), model, rules, expertSearch, a.logger)
		if err != nil {
			return built, err
		}
		built.expert = expert
		a.logger.Info("代理：看画面交给幕后专家，判断由前台转述", "agent", rules.Name, "model", model, "search", expertSearch != nil)
	}

	titleDefinition := a.catalog[agents.Title]
	titleModel, _, err := a.metered(titleDefinition)
	if err != nil {
		return built, err
	}
	title, err := a.agent(titleDefinition, spending.NewBudgetedChatModel(titleModel, a.guard, a.logger), chat.TitlePromptValues(), nil)
	if err != nil {
		return built, err
	}
	built.title = &title
	backstoryDefinition := a.catalog[agents.Backstory]
	backstoryModel, _, err := a.metered(backstoryDefinition)
	if err != nil {
		return built, err
	}
	pool, err := a.agent(backstoryDefinition, spending.NewBudgetedChatModel(backstoryModel, a.guard, a.logger), backstory.PromptValues(), nil)
	if err != nil {
		return built, err
	}
	built.backstory = &pool
	if s.ExpensesDB != "" {
		receiptDefinition := a.catalog[agents.Receipt]
		receiptModel, _, err := a.metered(receiptDefinition)
		if err != nil {
			return built, err
		}
		receipt, err := a.agent(receiptDefinition, spending.NewBudgetedChatModel(receiptModel, a.guard, a.logger), expenses.PromptValues(), nil)
		if err != nil {
			return built, err
		}
		built.receipt = &receipt
	}

	if s.ConversationSummary {
		definition := a.catalog[agents.Summary]
		if len(definition.Tools) > 0 { // 写摘要不调用工具：写了名单也不会生效，不如起不来
			return built, fmt.Errorf("代理 %s（%s）写摘要不用工具，tools 要留空：%v", definition.Name, definition.Source, definition.Tools)
		}
		summaryModel, _, err := a.metered(definition)
		if err != nil {
			return built, err
		}
		summary, err := a.agent(definition, spending.NewBudgetedChatModel(summaryModel, a.guard, a.logger), brain.SummaryPromptValues(), nil)
		if err != nil {
			return built, err
		}
		built.summary = &summary
	}

	if s.CanRaiseMonthlyCap() {
		definition := a.catalog[agents.OverBudget]
		judgeModel, _, err := a.metered(definition)
		if err != nil {
			return built, err
		}
		// 不受额度管（否则上限到了就没法判断），按天限次数、照样记账
		judge, err := a.agent(definition, spending.NewDailyCallLimit(judgeModel, s.OverBudgetDailyCalls),
			brain.OverBudgetPromptValues(definition.Tools), toolbox)
		if err != nil {
			return built, err
		}
		built.overBudget = &judge
	}
	return built, nil
}

// researchAgents 按定义装配后台调查的代理：调查（research）一定有；审查（research_review）见 ResearchReviewed。
// 调查的轮数默认是定义的 max_steps，设了 HARO_RESEARCH_MAX_ROUNDS 时用它；审查用哪个模型、审一次多久默认按定义，
// 设了 HARO_RESEARCH_REVIEWER、HARO_RESEARCH_REVIEW_TIMEOUT 时用它们。工具名按 research 自己提供的查（不在会话和
// 工具箱里），正文里的工具名、上限由 research 填。模型不套额度（调查自己每一轮之前查）、不熔断（后台做，不急）。
func (a agentAssembly) researchAgents(limits research.Limits) (research.Agents, error) {
	reviewed := a.settings.ResearchReviewed()
	definition := a.catalog[agents.Research]
	model, investigatorModel, err := a.meteredForResearch(definition)
	if err != nil {
		return research.Agents{}, err
	}
	rounds := cmp.Or(a.settings.ResearchMaxRounds, definition.MaxSteps)
	investigator, err := a.agentChecked(definition, model, research.InvestigationPromptValues(definition.Tools, rounds, limits, reviewed),
		unknownNames(definition.Tools, research.InvestigationTools()))
	if err != nil {
		return research.Agents{}, err
	}
	investigator.MaxSteps = rounds
	built := research.Agents{Investigator: investigator}
	reviewerModel := "（不审查）"
	if reviewed {
		definition := a.catalog[agents.ResearchReview]
		definition.Model = agents.TierBase // 审查用哪档只看 HARO_RESEARCH_REVIEWER（取值启动时查过）
		if a.settings.ResearchReviewer == config.ReviewerStrong {
			definition.Model = agents.TierStrong
		}
		definition.Timeout = cmp.Or(a.settings.ResearchReviewTimeout, definition.Timeout)
		model, id, err := a.meteredForResearch(definition)
		if err != nil {
			return research.Agents{}, err
		}
		reviewer, err := a.agentChecked(definition, model, research.ReviewPromptValues(definition.Tools),
			unknownNames(definition.Tools, research.ReviewTools()))
		if err != nil {
			return research.Agents{}, err
		}
		built.Reviewer, reviewerModel = &reviewer, id
	}
	a.logger.Info("代理：后台调查", "agent", investigator.Name, "model", investigatorModel, "max_rounds", rounds,
		"reviewer", reviewerModel, "effort", a.settings.ResearchEffort, "thinking", cmp.Or(a.settings.ResearchThinking, "（各用各的）"))
	return built, nil
}

// agent 按定义组装一个代理：model 是已经套好额度（和熔断）的模型，正文按 values 填好；定义里的工具名都要是会话或工具箱提供的。
func (a agentAssembly) agent(definition agents.Definition, model brain.ChatModel, values map[string]any, toolbox brain.Toolbox) (brain.Agent, error) {
	return a.agentChecked(definition, model, values, brain.UnknownTools(definition.Tools, toolbox))
}

// agentChecked 同 agent；unknown 是定义的允许名单里认不出的工具名（不为空就起不来）。
func (a agentAssembly) agentChecked(definition agents.Definition, model brain.ChatModel, values map[string]any, unknown []string) (brain.Agent, error) {
	instructions, err := definition.RenderPrompt(values)
	if err != nil {
		return brain.Agent{}, err
	}
	if len(unknown) > 0 {
		return brain.Agent{}, fmt.Errorf("代理 %s（%s）的工具名不认识：%s", definition.Name, definition.Source, strings.Join(unknown, "、"))
	}
	return brain.Agent{Name: definition.Title, Model: model, Instructions: instructions, Tools: definition.Tools,
		MaxSteps: definition.MaxSteps, RepeatLimit: definition.RepeatLimit, RepeatWindow: definition.RepeatWindow,
		Timeout: definition.Timeout}, nil
}

// unknownNames 是 names 里不在 known 中的名字。
func unknownNames(names, known []string) []string {
	var unknown []string
	for _, name := range names {
		if !slices.Contains(known, name) {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// metered 是按定义接好的具体模型（模型档换成具体模型，按代理的用途记账；不查额度、不熔断），以及模型 ID（日志用）。
// 用在对话里等着回答的代理上：力度、等多久按各档的配置，base 档跟着 HARO_LLM_PROVIDER（本地或云端）。
func (a agentAssembly) metered(definition agents.Definition) (brain.ChatModel, string, error) {
	return a.meteredAs(definition, false)
}

// meteredForResearch 同 metered，用在后台调查的代理上：只用云端模型（HARO_LLM_PROVIDER=local 时也是），力度用
// HARO_RESEARCH_EFFORT；在后台、不急：限流 / 服务端一时出错时让 SDK 重试 researchRetries 次，每次请求不另设期限
// （受调查自己的时限、审查的 timeout 管）。
func (a agentAssembly) meteredForResearch(definition agents.Definition) (brain.ChatModel, string, error) {
	return a.meteredAs(definition, true)
}

// researchRetries 是后台调查的请求在限流、服务端一时出错、断线时让 SDK 重试几次。
const researchRetries = 2

// meteredAs 是 metered、meteredForResearch 共用的部分：background 为 true 时按后台调查的做法接（见 meteredForResearch）。
func (a agentAssembly) meteredAs(definition agents.Definition, background bool) (brain.ChatModel, string, error) {
	purpose, err := spending.ParsePurpose(definition.Purpose)
	if err != nil {
		return nil, "", fmt.Errorf("代理 %s（%s）：%w", definition.Name, definition.Source, err)
	}
	s := a.settings
	maxTokens := cmp.Or(definition.MaxOutputTokens, s.CloudMaxTokens)
	logger := a.logger.With("agent", definition.Title)
	var spec cloudModelSpec
	switch {
	case definition.Model == agents.TierStrong:
		if s.StrongLLMModel == "" {
			return nil, "", fmt.Errorf("代理 %s 用更强的模型，但没设 HARO_STRONG_LLM_MODEL", definition.Name)
		}
		maxTokens = cmp.Or(definition.MaxOutputTokens, s.StrongLLMMaxTokens) // 它开着思考时思考也算在里面，比聊天的上限大
		spec = cloudModelSpec{model: s.StrongLLMModel, maxTokens: maxTokens, effort: s.StrongLLMEffort,
			thinking: s.StrongLLMThinking, firstTokenTimeout: s.StrongLLMTimeout, requestTimeout: s.RequestTimeout}
	case s.LLMProvider == config.LLMLocal && !background: // base 档、本地模型
		local, err := llm.NewOllamaChatModel(s.OllamaURL, s.Model, maxTokens, localModelOptions(s, a.client))
		if err != nil {
			return nil, "", err
		}
		// 每个具体模型都记账（本地模型在价格表里标了不花钱，照样记 token）
		return spending.NewMeteredChatModel(llm.WithRequestTelemetry(local, llm.ProviderOllama, s.Model), s.Model, purpose,
			a.pricing, a.ledger, logger), s.Model, nil
	default: // base 档、云端模型
		spec = cloudModelSpec{model: s.CloudLLMModel, maxTokens: maxTokens, effort: s.CloudLLMEffort,
			thinking: s.CloudLLMThinking, cacheTTL: s.CloudLLMCacheTTL, firstTokenTimeout: s.CloudLLMTimeout, requestTimeout: s.RequestTimeout}
	}
	if background {
		spec.effort, spec.retries, spec.firstTokenTimeout, spec.requestTimeout = s.ResearchEffort, researchRetries, 0, 0
		spec.cacheTTL = "" // 一步接一步地请求：5 分钟的缓存就够，1 小时的写缓存贵
		spec.thinking = cmp.Or(s.ResearchThinking, spec.thinking)
		spec.maxTokens = cmp.Or(s.ResearchMaxOutputTokens, spec.maxTokens)
	}
	return spending.NewMeteredChatModel(newCloudChatModel(s, a.claude, spec, a.client), spec.model, purpose, a.pricing, a.ledger, logger),
		spec.model, nil
}

// localModelOptions 是本地模型（ollama）的请求参数：等它开始回答按本地的上限（冷启动要先载入模型），不按云端的。
func localModelOptions(s config.ServerSettings, client *http.Client) llm.OpenAIOptions {
	return llm.OpenAIOptions{HTTPClient: client, FirstTokenTimeout: s.LocalLLMTimeout, RequestTimeout: s.RequestTimeout}
}
