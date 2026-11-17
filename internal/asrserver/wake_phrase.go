package asrserver

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// wakePhrase 匹配「哈罗哈罗」及其常见误识别写法（哈喽 / 哈啰 / Hello / Halo / 中英混写的「哈ello」「hell喽」…），
// 最多两遍，连同后面的标点。
//
// 为什么在文字上去掉唤醒词，而不是按时间截掉：唤醒词识别要在唤醒词说完一段时间后才报告，真实说话时这段延迟不固定，
// 按时间截要么切掉用户接着说的头几个字，要么留下唤醒词的尾音（2026-11-15 实测：尾音被识别成日语「あ？」，
// 只叫唤醒词也被当成了提问）。SenseVoice 对整句里的「哈罗哈罗」识别很稳，所以整句识别后再去掉。
var wakePhrase = regexp.MustCompile(
	`(?i)(?:\s*(?:哈\s*(?:[罗喽啰洛落咯]|h?e?l+o)|ha\s*l(?:o|ou|uo|ow)|hel+o|hel+\s*[喽罗啰洛落咯]|hal+o)\s*[，,。.!！?？、~～…\s]*){1,2}`)

// maxRunesBeforeWakePhrase：唤醒词前面最多还允许有几个字（例如「嗯，哈罗哈罗」），超过就认为不是句首的唤醒词。
const maxRunesBeforeWakePhrase = 4

// StripWakePhrase 去掉句首的唤醒词（连同它前面的少量内容）；去掉后只剩标点或空白时返回空字符串（表示「只叫了唤醒词」）。
// 只用于确实识别到唤醒词的那句话。
func StripWakePhrase(transcript string) string {
	rest, found := transcript, false
	if loc := wakePhrase.FindStringIndex(transcript); loc != nil && utf8.RuneCountInString(transcript[:loc[0]]) <= maxRunesBeforeWakePhrase {
		rest, found = transcript[loc[1]:], true
	}
	return emptyIfNothingSaid(strings.TrimSpace(rest), found)
}

// 唤醒词后面只剩语气词（「哈喽哈喽啊」「哈罗哈罗，嗯……」「Hello hello, um」）也算只叫了唤醒词：
// 2026-11-17 真机：剩下的「啊。」被当成一句话交给模型，模型顺着上一个话题讲了一大段，而人只是刚叫醒它。
const fillerRunes = "啊嗯呃哦噢喔诶欸哎唉呀哈嘿额唔嗨"

var (
	punctuationRunes = "，,。.!！?？、~～…- "
	englishFiller    = regexp.MustCompile(`^(?i:ah+|oh+|uh+|um+|hm+|eh+|er+|yeah|hey)$`)
)

// onlyFiller 判断去掉唤醒词后剩下的是不是只有标点和语气词。
func onlyFiller(rest string) bool {
	words := strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune(punctuationRunes, r) })
	for _, word := range words {
		if englishFiller.MatchString(word) {
			continue
		}
		for _, r := range word {
			if !strings.ContainsRune(fillerRunes, r) {
				return false
			}
		}
	}
	return true
}

// StripThroughWakePhrase 去掉唤醒词以及它前面的全部内容（打断时用，v3 I-1）：播放回答时被叫，
// 唤醒词前面混着 haro 自己的回声，不管有多长都不算用户说的。识别结果里找不到唤醒词时原样返回。
func StripThroughWakePhrase(transcript string) string {
	rest, found := transcript, false
	if loc := wakePhrase.FindStringIndex(transcript); loc != nil {
		rest, found = transcript[loc[1]:], true
	}
	return emptyIfNothingSaid(strings.TrimSpace(rest), found)
}

// emptyIfNothingSaid：只剩标点 / 空白时返回空；去掉了唤醒词时，只剩语气词也返回空。
func emptyIfNothingSaid(rest string, afterWakePhrase bool) string {
	words := strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune(punctuationRunes, r) })
	if len(words) == 0 || (afterWakePhrase && onlyFiller(rest)) {
		return ""
	}
	return rest
}
