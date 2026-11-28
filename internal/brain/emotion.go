package brain

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 情绪标记：模型在句首写「[调侃]」这样的标记，管到下一个标记为止（同一次回答之内）。
// 这是固定的语法：半角或全角方括号里是下面清单里的一个名字（括号内允许空白），按语法认，不在话里找词。
// 标记只用来选语气，不送去合成、不显示给终端；对话历史里保留模型的原话（含标记），让模型看到自己一直在这么写。
// 清单只在这里定义：认标记的规则、提示词里列出的标记都由它生成。

// emotionTag 是一个情绪标记。
type emotionTag struct {
	Label   string
	Emotion Emotion
}

// emotionTags 是全部情绪标记（提示词里按这个顺序列出）。
var emotionTags = []emotionTag{
	{"调侃", EmotionTease}, {"笑", EmotionLaugh}, {"激动", EmotionExcited}, {"温柔", EmotionGentle},
	{"轻声", EmotionWhisper}, {"平常", EmotionDefault}, {"撩", EmotionFlirt},
}

// emotionTagPattern 匹配一个情绪标记。
var emotionTagPattern = func() *regexp.Regexp {
	labels := make([]string, len(emotionTags))
	for i, tag := range emotionTags {
		labels[i] = regexp.QuoteMeta(tag.Label)
	}
	return regexp.MustCompile(`[\[【]\s*(` + strings.Join(labels, "|") + `)\s*[\]】]`)
}()

// emotionTagList 是提示词里列出的标记，例如「[调侃] [笑] …」。
func emotionTagList() string {
	tags := make([]string, len(emotionTags))
	for i, tag := range emotionTags {
		tags[i] = "[" + tag.Label + "]"
	}
	return strings.Join(tags, " ")
}

func emotionOf(label string) Emotion {
	for _, tag := range emotionTags {
		if tag.Label == label {
			return tag.Emotion
		}
	}
	return EmotionDefault
}

// emotionLabel 返回情绪的名字（日志用）。
func emotionLabel(emotion Emotion) string {
	for _, tag := range emotionTags {
		if tag.Emotion == emotion {
			return tag.Label
		}
	}
	return string(emotion)
}

// leadingTagPattern 匹配句首一个标记语法的片段（方括号里几个字）。清单里的标记已经被 emotionStream 取走，
// 句首还剩的就是清单外的：只记日志、照原样念，先看实际出现多少再定怎么处理。
var leadingTagPattern = regexp.MustCompile(`^\s*([\[【]\s*[^\]】\s]{1,6}\s*[\]】])`)

// leadingUnknownTag 返回句首清单外的标记（例如「[无奈]」）；没有时 ok 为 false。
func leadingUnknownTag(text string) (tag string, ok bool) {
	match := leadingTagPattern.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// stripEmotionTags 去掉文字里所有情绪标记（不经过切句、直接念的固定回答用）。
func stripEmotionTags(text string) string {
	return strings.TrimSpace(emotionTagPattern.ReplaceAllString(text, ""))
}

// WithoutEmotionTags 是给人看的文字：去掉情绪标记（历史里存的是模型的原话，App 的接口给出记录时用它）。
func WithoutEmotionTags(text string) string { return stripEmotionTags(text) }

// emotionStream 从模型的流式文字里认出情绪标记，在文字进切句器之前：标记处先把前面的文字收尾成句，
// 之后的句子换成新的语气（切句器只看到纯文字，标记不会被分句规则切开）。
type emotionStream struct{ pending string }

// emotionPiece 是认出来的一片：文字，或者一个标记。
type emotionPiece struct {
	text    string
	tag     bool    // 这一片是标记
	emotion Emotion // 标记：从这里起换成的语气
}

// Push 喂入新文字，返回按顺序认出来的片段。末尾可能是还没写完的标记（「[」「[笑」）：先留着，等后面的字。
func (e *emotionStream) Push(text string) []emotionPiece {
	e.pending += text
	var pieces []emotionPiece
	for {
		loc := emotionTagPattern.FindStringSubmatchIndex(e.pending)
		if loc == nil {
			break
		}
		if loc[0] > 0 {
			pieces = append(pieces, emotionPiece{text: e.pending[:loc[0]]})
		}
		pieces = append(pieces, emotionPiece{tag: true, emotion: emotionOf(e.pending[loc[2]:loc[3]])})
		e.pending = e.pending[loc[1]:]
	}
	if keep := unfinishedTagStart(e.pending); keep > 0 {
		pieces = append(pieces, emotionPiece{text: e.pending[:keep]})
		e.pending = e.pending[keep:]
	}
	return pieces
}

// Flush 交出剩下的文字（没写完的标记就当普通文字）。
func (e *emotionStream) Flush() []emotionPiece {
	rest := e.pending
	e.pending = ""
	if rest == "" {
		return nil
	}
	return []emotionPiece{{text: rest}}
}

// maxTagRunes 是一个标记最多几个字（括号、名字、括号里的空白）：超过就不可能是没写完的标记。
const maxTagRunes = 8

// unfinishedTagStart 返回末尾可能是没写完的标记的起点；没有时返回 len(text)（全部可以交出去）。
func unfinishedTagStart(text string) int {
	open := max(strings.LastIndex(text, "["), strings.LastIndex(text, "【"))
	if open < 0 || utf8.RuneCountInString(text[open:]) > maxTagRunes || strings.ContainsAny(text[open:], "]】") {
		return len(text)
	}
	_, size := utf8.DecodeRuneInString(text[open:])
	inside := strings.TrimSpace(text[open+size:])
	for _, tag := range emotionTags {
		if strings.HasPrefix(tag.Label, inside) || strings.HasPrefix(inside, tag.Label) {
			return open
		}
	}
	return len(text)
}

// Medium 是主人和 haro 怎么说话：语音（终端上说）还是打字（App 里）。前台的说话格式按它分开。
type Medium string

// 两种说话方式。
const (
	MediumVoice Medium = "voice"
	MediumText  Medium = "text"
)

// PromptValuesFor 是代理正文里可以引用的、由 brain 定义的值：{{.EmotionTags}}（情绪标记的列表只在这里定义一处）、
// {{.Medium}}（语音还是打字）。
func PromptValuesFor(medium Medium) map[string]any {
	return map[string]any{"EmotionTags": emotionTagList(), "Medium": string(medium)}
}

// PromptValues 是语音用的 PromptValuesFor。
func PromptValues() map[string]any { return PromptValuesFor(MediumVoice) }
