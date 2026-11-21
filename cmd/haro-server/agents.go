package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
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
		if definition := catalog[name]; !definition.Builtin() {
			logger.Warn("代理定义：用覆盖目录里的这份（程序里带的那份以后更新了也不会跟着变）", "agent", name, "file", definition.Source)
		}
	}
	return catalog, nil
}

// agentAssembly 是装配代理要用的东西。
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

// conversationAgents 是对话用的代理。
type conversationAgents struct {
	front   brain.Agent
	handoff brain.Handoff     // 主人纠正时（或看画面接手时）接手的强模型；没配时为零值
	expert  brain.FrameExpert // 看画面的专家；没配时为 nil
	summary *brain.Agent      // 写滚动摘要；没开时为 nil
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
	if s.ExtraSystemPrompt != "" {
		front.Instructions += "\n\n" + s.ExtraSystemPrompt
	}
	built.front = front

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

	if s.ConversationSummary {
		definition := a.catalog[agents.Summary]
		if len(definition.Tools) > 0 { // 写摘要不调用工具：写了名单也不会生效，不如起不来
			return built, fmt.Errorf("代理 %s（%s）写摘要不用工具，tools 要留空：%v", definition.Name, definition.Source, definition.Tools)
		}
		summaryModel, _, err := a.metered(definition)
		if err != nil {
			return built, err
		}
		summary, err := a.agent(definition, spending.NewBudgetedChatModel(summaryModel, a.guard, a.logger), nil, nil)
		if err != nil {
			return built, err
		}
		built.summary = &summary
	}
	return built, nil
}

// agent 按定义组装一个代理：model 是已经套好额度（和熔断）的模型，正文按 values 填好；定义里的工具名都要认识。
func (a agentAssembly) agent(definition agents.Definition, model brain.ChatModel, values map[string]any, toolbox brain.Toolbox) (brain.Agent, error) {
	instructions, err := definition.RenderPrompt(values)
	if err != nil {
		return brain.Agent{}, err
	}
	if unknown := brain.UnknownTools(definition.Tools, toolbox); len(unknown) > 0 {
		return brain.Agent{}, fmt.Errorf("代理 %s（%s）的工具名不认识：%s", definition.Name, definition.Source, strings.Join(unknown, "、"))
	}
	return brain.Agent{Name: definition.Title, Model: model, Instructions: instructions, Tools: definition.Tools,
		MaxSteps: definition.MaxSteps, RepeatLimit: definition.RepeatLimit, RepeatWindow: definition.RepeatWindow}, nil
}

// metered 是按定义接好的具体模型（模型档换成具体模型，按代理的用途记账；不查额度、不熔断），以及模型 ID（日志用）。
func (a agentAssembly) metered(definition agents.Definition) (brain.ChatModel, string, error) {
	purpose, err := spending.ParsePurpose(definition.Purpose)
	if err != nil {
		return nil, "", fmt.Errorf("代理 %s（%s）：%w", definition.Name, definition.Source, err)
	}
	s := a.settings
	maxTokens := definition.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = s.CloudMaxTokens
	}
	logger := a.logger.With("agent", definition.Title)
	var model brain.ChatModel
	var id string
	switch {
	case definition.Model == agents.TierStrong:
		if s.StrongLLMModel == "" {
			return nil, "", fmt.Errorf("代理 %s 用更强的模型，但没设 HARO_STRONG_LLM_MODEL", definition.Name)
		}
		id = s.StrongLLMModel
		model = newCloudChatModel(s, a.claude, cloudModelSpec{model: id, maxTokens: maxTokens, effort: s.StrongLLMEffort,
			thinking: s.StrongLLMThinking, firstTokenTimeout: s.StrongLLMTimeout, requestTimeout: s.RequestTimeout}, a.client)
	case s.LLMProvider == config.LLMLocal: // base 档、本地模型
		id = s.Model
		model = llm.WithRequestTelemetry(llm.NewOllamaChatModel(s.OllamaURL, id, maxTokens, llm.OpenAIOptions{HTTPClient: a.client,
			FirstTokenTimeout: s.CloudLLMTimeout, RequestTimeout: s.RequestTimeout}), llm.ProviderOllama, id)
	default: // base 档、云端模型
		id = s.CloudLLMModel
		model = newCloudChatModel(s, a.claude, cloudModelSpec{model: id, maxTokens: maxTokens, effort: s.CloudLLMEffort,
			thinking: s.CloudLLMThinking, firstTokenTimeout: s.CloudLLMTimeout, requestTimeout: s.RequestTimeout}, a.client)
	}
	// 每个具体模型都记账（本地模型在价格表里标了不花钱，照样记 token）
	return spending.NewMeteredChatModel(model, id, purpose, a.pricing, a.ledger, logger), id, nil
}
