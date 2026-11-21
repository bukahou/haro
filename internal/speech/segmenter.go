package speech

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// SentenceSegmenter 把模型的流式文字切成适合逐句合成的句子。边界都按 Unicode 的文本分段规则（UAX #29，rivo/uniseg），
// 不按某种语言的标点表：
//   - 句子边界：中日文的句号、问号、叹号之后，英文句点后面跟着空格和下一句时，换行处；小数点、缩写里的点不算。
//     边界后面还没来文字时，看这个边界确不确定（后面不管来什么字都在这里断开，例如句号；英文句点要等下一个字）：
//     确定就马上交出去（调用工具前说的「我查一下」不用等工具调用写完）；
//     代价是紧跟在句号后面的引号、括号会分到下一句（不念出来，不影响念的内容）；
//   - 第一句碰到可以停顿的地方（单独成词的标点，例如逗号、顿号、冒号；「1,000」「don't」里的不算），
//     且已攒够 firstMinRunes 个字，就提前切，让第一个字更早出来；
//   - 之后超过 maxRunes 个字还没有句子边界，就在最后一个可以停顿的地方切（没有就硬切）；
//   - 有效字不到 2 个的片段并到下一句；绝不输出空句（Piper 对空文本返回 500）。
//
// 三种切点按文字的先后取最早的那个：同一段文字不管怎么分块喂进来，念出来的都一样。
type SentenceSegmenter struct {
	firstMinRunes int // 0 = 不提前切
	maxRunes      int
	pending       string
	emitted       int
}

const minSegmentRunes = 2

// NewSentenceSegmenter 创建切句器。
func NewSentenceSegmenter(firstMinRunes, maxRunes int) *SentenceSegmenter {
	return &SentenceSegmenter{firstMinRunes: firstMinRunes, maxRunes: maxRunes}
}

// Push 喂入新文字，返回已经完整的句子。
func (s *SentenceSegmenter) Push(text string) []string {
	s.pending += text
	var sentences []string
	for {
		end := s.nextCut()
		if end <= 0 {
			return sentences
		}
		sentences = append(sentences, cleanForSpeech(s.pending[:end]))
		s.pending = s.pending[end:]
		s.emitted++
	}
}

// Flush 返回剩余内容（只要还有可读的字）。
func (s *SentenceSegmenter) Flush() []string {
	rest := cleanForSpeech(s.pending)
	s.pending = ""
	if countSpokenRunes(rest) == 0 {
		return nil
	}
	s.emitted++
	return []string{rest}
}

// nextCut 返回下一句在 pending 里的结束位置（字节）；还没有能切的地方时返回 -1。
func (s *SentenceSegmenter) nextCut() int {
	cut := s.sentenceEnd()
	for _, candidate := range []int{s.earlyCut(), s.longCut()} {
		if candidate > 0 && (cut < 0 || candidate < cut) {
			cut = candidate
		}
	}
	return cut
}

// sentenceEnd 是第一个累计够 minSegmentRunes 个有效字的句子边界（太短的句子并到下一句）；
// 边界后面还没有文字时，这个边界要确定才算（见 certainEnd）。
func (s *SentenceSegmenter) sentenceEnd() int {
	end, rest, state := 0, s.pending, -1
	for {
		sentence, next, newState := uniseg.FirstSentenceInString(rest, state)
		if next == "" && !certainEnd(sentence, state) { // 这一句后面还没有文字，也不确定已经完了
			return -1
		}
		end += len(sentence)
		rest, state = next, newState
		if countSpokenRunes(s.pending[:end]) >= minSegmentRunes {
			return end
		}
		if next == "" {
			return -1
		}
	}
}

// sentenceProbes 是试探用的下一个字：小写、大写、数字、汉字。
var sentenceProbes = []string{"a", "A", "5", "中"}

// certainEnd 判断 sentence（后面暂时没有文字）是不是已经完了：后面不管接哪种字，分句规则都在它的末尾断开。
func certainEnd(sentence string, state int) bool {
	for _, probe := range sentenceProbes {
		if first, _, _ := uniseg.FirstSentenceInString(sentence+probe, state); len(first) != len(sentence) {
			return false
		}
	}
	return true
}

// earlyCut 是第一句提前切的位置：第一个前面已经攒够 firstMinRunes 个字的停顿处。只对第一句。
func (s *SentenceSegmenter) earlyCut() int {
	if s.emitted > 0 || s.firstMinRunes <= 0 {
		return -1
	}
	for _, at := range pausePoints(s.pending) {
		if countSpokenRunes(s.pending[:at]) >= s.firstMinRunes {
			return at
		}
	}
	return -1
}

// longCut 是超过 maxRunes 个字还没断开时的切点：超出的那一刻之前最后一个停顿处，没有就在那一刻硬切。
func (s *SentenceSegmenter) longCut() int {
	if s.maxRunes <= 0 {
		return -1
	}
	spoken := 0
	for at, r := range s.pending {
		if !isSpoken(r) {
			continue
		}
		if spoken++; spoken > s.maxRunes {
			exceeded := at + utf8.RuneLen(r)
			cut := exceeded
			for _, pause := range pausePoints(s.pending[:exceeded]) {
				cut = pause
			}
			return cut
		}
	}
	return -1
}

// pausePoints 是 text 里可以停顿的位置（单独成词的标点之后）。最后面那个标点不算：后面的文字还没来，
// 它可能是「1,000」「don't」这样的词的一部分。
func pausePoints(text string) []int {
	var points []int
	end, rest, state := 0, text, -1
	for rest != "" {
		word, next, newState := uniseg.FirstWordInString(rest, state)
		end += len(word)
		rest, state = next, newState
		if next != "" && isPunctuationWord(word) {
			points = append(points, end)
		}
	}
	return points
}

// isPunctuationWord 判断一个分词结果是不是只有一个标点（Unicode 的 P 类）。
func isPunctuationWord(word string) bool {
	r, size := utf8.DecodeRuneInString(word)
	return size == len(word) && unicode.IsPunct(r)
}

// isSpoken 判断一个字会不会被读出来（字母、汉字、数字）。
func isSpoken(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// countSpokenRunes 统计会被读出来的字。
func countSpokenRunes(text string) int {
	count := 0
	for _, r := range text {
		if isSpoken(r) {
			count++
		}
	}
	return count
}

// cleanForSpeech 去掉 Markdown 标记和表情，合并空白。
// Markdown 是固定的标记语法（和语言无关），它的标记字符列在这里；表情按 Unicode 的区块认（Go 标准库没有表情属性表）。
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
