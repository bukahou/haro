package brain

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// 性格档案。
//
// 系统提示词 = 前台代理定义里的基础提示词（身份、能力、底线规则，所有性格共用）+ 当前性格的说话风格。
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

// PersonaState 记住当前性格，重启后保持。
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

// findPersona 按编号或名字找性格（完全一致才算；主人说的是哪一个由模型判断，按编号交过来）。
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
	return nil
}

type switchPersonaInput struct {
	Persona string `json:"persona" jsonschema:"要换成的性格的编号"`
}

// switchPersonaSpec 生成切换工具的说明：可选的性格按编号列出，名字和别名写在说明里给模型对照。只有一个性格时不提供这个工具。
func switchPersonaSpec(personas []Persona) (ToolSpec, bool) {
	if len(personas) < 2 {
		return ToolSpec{}, false
	}
	schema, err := jsonschema.For[switchPersonaInput](nil)
	if err != nil {
		panic(fmt.Sprintf("switch_persona 的参数定义: %v", err))
	}
	options := make([]string, 0, len(personas))
	for _, persona := range personas {
		schema.Properties["persona"].Enum = append(schema.Properties["persona"].Enum, persona.ID)
		option := persona.ID + "（" + persona.Name
		if len(persona.Aliases) > 0 {
			option += "，也叫" + strings.Join(persona.Aliases, "、")
		}
		options = append(options, option+"）")
	}
	parameters, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("switch_persona 的参数定义: %v", err))
	}
	return ToolSpec{Name: switchPersonaTool, Parameters: parameters,
		Description: "切换你的性格（说话方式和语气）。只有主人明确要求换性格、换模式、换说话风格时才调用，" +
			"例如「换成温柔模式」「变回毒舌的」。可选（编号和名字）：" + strings.Join(options, "；") + "。"}, true
}

// currentPersona 返回当前性格（可能为 nil）。
func (s *Session) currentPersona() *Persona {
	return s.currentConversation().currentPersona()
}

// switchPersona 执行 switch_persona，返回交给模型的结果文字。
func (s *Session) switchPersona(arguments map[string]any) string {
	in, err := DecodeArguments[switchPersonaInput](arguments)
	if err != nil {
		return "没有切换：" + err.Error()
	}
	persona := findPersona(s.opts.Personas, in.Persona)
	if persona == nil {
		s.logger.Info("调用工具", "tool", switchPersonaTool, "persona", in.Persona, "err", "没有这个性格")
		return fmt.Sprintf("没有编号为「%s」的性格，没有切换。", in.Persona)
	}
	s.currentConversation().setPersona(persona) // 只换这个人的
	if s.opts.PersonaState != nil {             // 也记成全家「上次选的」：新的人从它开始
		if err := s.opts.PersonaState.SavePersona(persona.ID); err != nil {
			s.logger.Warn("记住当前性格失败（重启后会回到默认）", "err", err)
		}
	}
	s.logger.Info("调用工具", "tool", switchPersonaTool, "persona", persona.Name)
	return fmt.Sprintf("已切换为「%s」，从这句起按新的性格说话。", persona.Name)
}

// systemPrompt 拼出系统提示词：前台的基础规则 + 工具介绍 + 主人资料 + 当前性格的风格（+ 之前聊过的摘要）。
func (s *Session) systemPrompt() string {
	s.mu.Lock()
	conversation := s.conversation
	s.mu.Unlock()
	profile := ""
	if s.parts.Profiles != nil {
		profile = s.parts.Profiles.Profile(conversation.Person())
	}
	prompt := composeSystemPrompt(s.parts.Front.Instructions, toolOverview(s.offeredTools()), profile, conversation.currentPersona())
	if note := summaryNote(conversation.currentSummary()); note != "" {
		prompt += "\n\n" + note
	}
	return prompt
}

// userTurn 是这一轮交给模型的「主人的话」：前面附上现在的情况（时间、位置、谁在说话、额度、任务板）。
//
// 为什么不放进系统提示词：这些每轮都变，而系统提示词在最前面——放在那里，每轮的开头都不一样，
// 支持提示词缓存的模型（按开头复用）就永远命中不了，本地模型也得从头处理整段历史。
// 放在最后一条消息里，系统提示词和历史保持不变；历史里只存主人的原话，不存这段附注。
// 情况说明单独成一条（标 Synthetic），不和主人的话拼在一起：最后一条始终是主人的原话。
// 要「说完才算告诉了」的情况（任务板的结论、额度提醒）记进 record，这一轮说完时才确认（closeTurn）。
func (s *Session) userTurn(text string, record *turnRecord) (situation []ChatMessage, input ChatMessage) {
	input = ChatMessage{Role: RoleUser, Text: text}
	if briefing := s.briefing(s.currentConversation(), record); briefing != "" {
		situation = []ChatMessage{{Role: RoleUser, Text: briefing, Synthetic: true}}
	}
	return situation, input
}

// conversationUnreadableNote 是之前的对话这一轮没读出来时（存储出错，见 Conversation.detached）给模型的附注。
const conversationUnreadableNote = "存对话的地方这一轮读不出来：你不记得之前聊过什么，这一轮说的也记不下来。" +
	"主人提到之前的事、或者要你记住什么时，照实告诉他现在记不住，不要编。"

// briefing 汇总这一轮要补充的情况：谁在说话（之前的对话没读出来时说明）+ 各 Briefer。
func (s *Session) briefing(conversation *Conversation, record *turnRecord) string {
	person := conversation.Person()
	var lines []string
	if person != "" && person != SharedPerson {
		lines = append(lines, "现在说话的是："+person+"（按声纹认出）。")
	}
	if conversation.detached {
		lines = append(lines, conversationUnreadableNote)
	}
	lines = append(lines, record.extraNotes...)
	for _, briefer := range s.parts.Briefers {
		briefing := briefer.Brief(person)
		lines = append(lines, briefing.Lines...)
		if briefing.Delivered != nil {
			record.delivered = append(record.delivered, briefing.Delivered)
		}
		if briefing.Interrupted != nil {
			record.interrupted = append(record.interrupted, briefing.Interrupted)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return situationHeading + joinLines(lines)
}

// situationHeading 是情况说明的开头。
const situationHeading = "【现在的情况（系统附注，不是主人说的话）】\n"

func joinLines(lines []string) string { return strings.Join(lines, "\n") }

// composeSystemPrompt 的顺序：内置规矩（+ 外部追加）→ 主人资料 → 性格。
// 规矩在最前面优先级最高；这几段都很少变，每轮的即时情况不在这里（见 userTurn）。
// 有没有摄像头不在这里：看画面的说明随 look_at_camera 工具一起出现或消失，系统提示词不变。
func composeSystemPrompt(instructions, tools, profile string, persona *Persona) string {
	parts := make([]string, 0, 4)
	for _, part := range []string{instructions, tools, profile, personaPrompt(persona)} {
		if part != "" {
			parts = append(parts, part)
		}
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
	emotion := Emotion(name)
	var names []string
	for _, tag := range emotionTags { // 情绪清单只在 emotionTags 定义
		if tag.Emotion == emotion {
			return emotion, nil
		}
		if tag.Emotion != EmotionDefault {
			names = append(names, string(tag.Emotion))
		}
	}
	return "", fmt.Errorf("不认识的情绪 %q（只能是 %s，或留空）", name, strings.Join(names, " / "))
}
