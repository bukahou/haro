package brain

import (
	"regexp"
	"strings"
	"unicode"
)

// 情绪标记：模型在句首写「[调侃]」这样的标记，标记管到下一个标记为止（同一次回答之内）。
// 标记只用来选语气，不送去合成、不显示给终端；对话历史里保留模型的原话（含标记），让模型看到自己一直在这么写。

var emotionTags = map[string]Emotion{
	"平常": EmotionDefault,
	"调侃": EmotionTease,
	"笑":  EmotionLaugh,
	"激动": EmotionExcited,
	"温柔": EmotionGentle,
	"轻声": EmotionWhisper,
	"撩":  EmotionFlirt,
}

// emotionTagPattern 匹配半角或全角方括号里的情绪名（括号内允许空白）。
var emotionTagPattern = regexp.MustCompile(`[\[【]\s*(平常|调侃|笑|激动|温柔|轻声|撩)\s*[\]】]`)

// takeEmotion 去掉一句话里的情绪标记，返回干净的文字和这句的情绪。
// 一句里有多个标记时以最后一个为准；没有标记就沿用 current（上一句的情绪）。
func takeEmotion(sentence string, current Emotion) (string, Emotion) {
	matches := emotionTagPattern.FindAllStringSubmatch(sentence, -1)
	if len(matches) == 0 {
		return sentence, current
	}
	emotion := emotionTags[matches[len(matches)-1][1]]
	return strings.TrimSpace(emotionTagPattern.ReplaceAllString(sentence, "")), emotion
}

// stripEmotionTags 去掉文字里所有情绪标记（固定回答、确认问句等不经过切句的文字用）。
func stripEmotionTags(text string) string {
	return strings.TrimSpace(emotionTagPattern.ReplaceAllString(text, ""))
}

// detectLanguage 按文字判断语言：含平假名或片假名 ⇒ 日语，否则中文。
// 只看假名：日语句子几乎都有假名，中文句子不会有；汉字两边都有，判断不了。
func detectLanguage(text string) Language {
	for _, r := range text {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana) && r != 'ー' && r != '・' {
			return LanguageJapanese
		}
	}
	return LanguageChinese
}

// emotionLabel 返回情绪的中文名（日志用）。
func emotionLabel(emotion Emotion) string {
	for label, value := range emotionTags {
		if value == emotion {
			return label
		}
	}
	return string(emotion)
}
