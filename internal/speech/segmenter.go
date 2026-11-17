package speech

import (
	"strings"
	"unicode"
)

// SentenceSegmenter 把模型的流式文字切成适合逐句合成的句子。
//   - 硬边界（。！？；… !?; 换行）碰到就切；
//   - 第一句碰到软边界（，、,：:）且已攒够 firstMinRunes 个字就提前切，让第一个字更早出来；
//   - 之后超过 maxRunes 个字还没有硬边界，就在最后一个软边界处切（没有就硬切）；
//   - 有效字不到 2 个的片段并到下一句；绝不输出空句（Piper 对空文本返回 500）。
//
// 半角句点 '.' 不算边界：中文回答里它几乎只出现在小数里（3.5）。
type SentenceSegmenter struct {
	firstMinRunes int // 0 = 不提前切
	maxRunes      int
	pending       []rune
	emitted       int
}

const minSegmentRunes = 2

// NewSentenceSegmenter 创建切句器。
func NewSentenceSegmenter(firstMinRunes, maxRunes int) *SentenceSegmenter {
	return &SentenceSegmenter{firstMinRunes: firstMinRunes, maxRunes: maxRunes}
}

// Push 喂入新文字，返回已经完整的句子。
func (s *SentenceSegmenter) Push(text string) []string {
	var sentences []string
	for _, r := range text {
		s.pending = append(s.pending, r)
		switch {
		case isHardBoundary(r):
			sentences = s.cut(len(s.pending), sentences)
		case isSoftBoundary(r) && s.emitted == 0 && s.firstMinRunes > 0 &&
			countSpokenRunes(s.pending) >= s.firstMinRunes:
			sentences = s.cut(len(s.pending), sentences)
		case s.maxRunes > 0 && countSpokenRunes(s.pending) > s.maxRunes:
			at := lastSoftBoundary(s.pending)
			if at == 0 {
				at = len(s.pending)
			}
			sentences = s.cut(at, sentences)
		}
	}
	return sentences
}

// Flush 返回剩余内容（只要还有可读的字）。
func (s *SentenceSegmenter) Flush() []string {
	rest := cleanForSpeech(string(s.pending))
	s.pending = nil
	if countSpokenRunes([]rune(rest)) == 0 {
		return nil
	}
	s.emitted++
	return []string{rest}
}

// cut 把 pending[:n] 作为候选句；有效字太少就留在 pending 里，和后面的内容合并。
func (s *SentenceSegmenter) cut(n int, sentences []string) []string {
	candidate := cleanForSpeech(string(s.pending[:n]))
	if countSpokenRunes([]rune(candidate)) < minSegmentRunes {
		return sentences
	}
	s.pending = append([]rune(nil), s.pending[n:]...)
	s.emitted++
	return append(sentences, candidate)
}

func isHardBoundary(r rune) bool { return strings.ContainsRune("。！？；…!?;\n", r) }

func isSoftBoundary(r rune) bool { return strings.ContainsRune("，、,：:", r) }

// lastSoftBoundary 返回最后一个软边界之后的位置；没有则返回 0。
func lastSoftBoundary(runes []rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if isSoftBoundary(runes[i]) {
			return i + 1
		}
	}
	return 0
}

// countSpokenRunes 统计会被读出来的字（字母、汉字、数字）。
func countSpokenRunes(runes []rune) int {
	count := 0
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			count++
		}
	}
	return count
}

// cleanForSpeech 去掉 Markdown 符号和表情，合并空白。
func cleanForSpeech(text string) string {
	var builder strings.Builder
	lastSpace := false
	for _, r := range text {
		switch {
		case strings.ContainsRune("*#`_~>|", r), isEmojiRune(r):
			continue
		case unicode.IsSpace(r):
			if !lastSpace {
				builder.WriteRune(' ')
			}
			lastSpace = true
			continue
		}
		builder.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(builder.String())
}

func isEmojiRune(r rune) bool {
	// 不用 unicode.So 整类：℃、© 这类要读出来的符号也在里面。
	return (r >= 0x1F000 && r <= 0x1FAFF) || // 表情、象形符号
		(r >= 0x2600 && r <= 0x27BF) || // 杂项符号、装饰符号（☀ ✅ 等）
		r == 0xFE0F || r == 0x200D // 变体选择符、零宽连接符
}
