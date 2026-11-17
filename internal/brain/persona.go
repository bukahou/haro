package brain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 性格档案。
//
// 系统提示词 = 终端传来的基础提示词（身份、能力、底线规则，所有性格共用）+ 当前性格的说话风格。
// 底线规则写在基础提示词里，性格档案只管风格，覆盖不了底线。
// 切换由模型调用内置工具 switch_persona 完成（和 look_at_camera 一样，因为它改的是会话状态）。

// Persona 是一个性格：说话风格 + 默认语气（没打情绪标记的句子用哪种情绪说）。
type Persona struct {
	ID             string
	Name           string   // 「毒舌搭子」
	Aliases        []string // 切换时也认这些叫法：「毒舌」「默认」
	Prompt         string   // 说话风格，接在基础提示词后面
	DefaultEmotion Emotion  // 没打标记（或标「平常」）的句子用的语气
}

// PersonaState 记住当前性格，重启后保持（P-3）。
type PersonaState interface {
	LoadPersona() (string, error) // 没记过时返回 ""
	SavePersona(id string) error
}

const switchPersonaTool = "switch_persona"

// initialPersona 选出会话开始时的性格：记住的 → 默认 → 第一个。没有性格档案时返回 nil。
func initialPersona(opts Options) (*Persona, error) {
	if len(opts.Personas) == 0 {
		return nil, nil
	}
	if opts.PersonaState != nil {
		id, err := opts.PersonaState.LoadPersona()
		if err != nil {
			return nil, fmt.Errorf("读取上次的性格: %w", err)
		}
		if persona := findPersona(opts.Personas, id); persona != nil {
			return persona, nil
		}
	}
	if persona := findPersona(opts.Personas, opts.DefaultPersona); persona != nil {
		return persona, nil
	}
	return &opts.Personas[0], nil
}

// findPersona 按 ID、名字或别名找性格（名字和别名允许包含关系：「温柔模式」能找到「温柔陪伴」的别名「温柔」）。
func findPersona(personas []Persona, query string) *Persona {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	for i := range personas {
		if personas[i].ID == query || personas[i].Name == query {
			return &personas[i]
		}
	}
	for i := range personas {
		for _, alias := range append([]string{personas[i].Name}, personas[i].Aliases...) {
			if alias != "" && (strings.Contains(query, alias) || strings.Contains(alias, query)) {
				return &personas[i]
			}
		}
	}
	return nil
}

// switchPersonaSpec 生成切换工具的说明（列出可选的性格）。只有一个性格时不提供这个工具。
func switchPersonaSpec(personas []Persona) (ToolSpec, bool) {
	if len(personas) < 2 {
		return ToolSpec{}, false
	}
	names := make([]string, 0, len(personas))
	for _, persona := range personas {
		names = append(names, persona.Name)
	}
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "enum": names, "description": "要换成的性格"},
		},
		"required": []string{"name"},
	})
	return ToolSpec{Name: switchPersonaTool, Parameters: parameters,
		Description: "切换你的性格（说话方式和语气）。只有主人明确要求换性格、换模式、换说话风格时才调用，" +
			"例如「换成温柔模式」「变回毒舌的」。可选：" + strings.Join(names, "、") + "。"}, true
}

// currentPersona 返回当前性格（可能为 nil）。
func (s *Session) currentPersona() *Persona {
	return s.currentConversation().currentPersona()
}

// switchPersona 执行 switch_persona，返回交给模型的结果文字。
func (s *Session) switchPersona(arguments map[string]any) string {
	name, _ := arguments["name"].(string)
	persona := findPersona(s.opts.Personas, name)
	if persona == nil {
		s.logger.Info("调用工具", "tool", switchPersonaTool, "name", name, "err", "没有这个性格")
		return fmt.Sprintf("没有叫「%s」的性格，没有切换。", name)
	}
	s.currentConversation().setPersona(persona) // 只换这个人的（v4 C-1）
	if s.opts.PersonaState != nil {             // 也记成全家「上次选的」：新的人从它开始
		if err := s.opts.PersonaState.SavePersona(persona.ID); err != nil {
			s.logger.Warn("记住当前性格失败（重启后会回到默认）", "err", err)
		}
	}
	s.logger.Info("调用工具", "tool", switchPersonaTool, "persona", persona.Name)
	return fmt.Sprintf("已切换为「%s」，从这句起按新的性格说话。", persona.Name)
}

// systemPrompt 拼出系统提示词：基础提示词 + 当前性格的风格。
func (s *Session) systemPrompt() string {
	s.mu.Lock()
	instructions, noCamera, conversation := s.instructions, s.noCamera || s.cameraGone, s.conversation
	s.mu.Unlock()
	profile := ""
	if s.parts.Profiles != nil {
		profile = s.parts.Profiles.Profile(conversation.Person())
	}
	prompt := composeSystemPrompt(instructions, profile, conversation.currentPersona(), noCamera)
	if note := summaryNote(conversation.currentSummary()); note != "" {
		prompt += "\n\n" + note
	}
	return prompt
}

// userTurn 是这一轮交给模型的「主人的话」：前面附上现在的情况（时间、位置、谁在说话、额度、任务板）。
//
// 为什么不放进系统提示词：这些每轮都变，而系统提示词在最前面——放在那里，每轮的开头都不一样，
// 支持提示词缓存的模型（按开头复用）就永远命中不了，本地模型也得从头处理整段历史（2026-11-17）。
// 放在最后一条消息里，系统提示词和历史保持不变；历史里只存主人的原话，不存这段附注。
func (s *Session) userTurn(text string) ChatMessage {
	briefing := s.briefing(s.currentConversation().Person())
	if briefing == "" {
		return ChatMessage{Role: RoleUser, Text: text}
	}
	return ChatMessage{Role: RoleUser, Text: briefing + "\n【主人说】\n" + text}
}

// briefing 汇总这一轮要补充的情况：谁在说话 + 各 Briefer（v5）。
func (s *Session) briefing(person string) string {
	var lines []string
	if person != "" && person != SharedPerson {
		lines = append(lines, "现在说话的是："+person+"（按声纹认出）。")
	}
	for _, briefer := range s.parts.Briefers {
		lines = append(lines, briefer.Brief(person)...)
	}
	if len(lines) == 0 {
		return ""
	}
	return "【现在的情况（系统附注，不是主人说的话）】\n" + strings.Join(lines, "\n")
}

// noCameraNote 接在系统提示词最后：基础提示词里写了「能用 look_at_camera 看画面」，没有摄像头时要更正。
const noCameraNote = "注意：现在这个终端没有摄像头，你看不到任何画面，也没有 look_at_camera 可用；主人问看得见的东西时，直接说现在看不到。"

// composeSystemPrompt 的顺序：内置规矩（+ 外部追加）→ 主人资料 → 性格 → 没有摄像头的更正。
// 规矩在最前面优先级最高；这几段都很少变，每轮的即时情况不在这里（见 userTurn）。
func composeSystemPrompt(instructions, profile string, persona *Persona, noCamera bool) string {
	parts := make([]string, 0, 4)
	for _, part := range []string{instructions, profile, personaPrompt(persona)} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if noCamera {
		parts = append(parts, noCameraNote)
	}
	return strings.Join(parts, "\n\n")
}

func personaPrompt(persona *Persona) string {
	if persona == nil {
		return ""
	}
	return persona.Prompt
}

// voiceEmotion 把句子的情绪落到声音上：没打标记（平常）时用当前性格的默认语气。
func (s *Session) voiceEmotion(emotion Emotion) Emotion {
	if emotion != EmotionDefault {
		return emotion
	}
	if persona := s.currentPersona(); persona != nil {
		return persona.DefaultEmotion
	}
	return EmotionDefault
}

// ParseEmotion 把配置文件里的情绪名（tease / gentle …，空 = 平常）转成 Emotion；不认识的报错。
func ParseEmotion(name string) (Emotion, error) {
	switch emotion := Emotion(name); emotion {
	case EmotionDefault, EmotionTease, EmotionLaugh, EmotionExcited, EmotionGentle, EmotionWhisper, EmotionFlirt:
		return emotion, nil
	}
	return "", fmt.Errorf("不认识的情绪 %q（只能是 tease / laugh / excited / gentle / whisper / flirt，或留空）", name)
}
