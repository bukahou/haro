package speech

import (
	"slices"
	"strings"
	"testing"
)

func TestSentenceSegmenter(t *testing.T) {
	thirty := strings.Repeat("一", 30)
	fifteen := strings.Repeat("二", 15)
	cases := []struct {
		name          string
		firstMinRunes int
		input         string
		want          []string
	}{
		{"硬边界切句", 4, "你好，我是哈罗。我能看到你。",
			[]string{"你好，我是哈罗。", "我能看到你。"}},
		{"第一句在逗号处提前切", 4, "我看到你坐在电脑前面，旁边有一个杯子。",
			[]string{"我看到你坐在电脑前面，", "旁边有一个杯子。"}},
		{"提前切的停顿处在句号之前：先在停顿处切（后面还有一句也一样）", 4, "我看到你坐在电脑前面，旁边有一个杯子。还有键盘。",
			[]string{"我看到你坐在电脑前面，", "旁边有一个杯子。", "还有键盘。"}},
		{"只有第一句提前切", 4, "好的。我看到你坐在电脑前面，旁边有杯子。",
			[]string{"好的。", "我看到你坐在电脑前面，旁边有杯子。"}},
		{"太短的并到下一句", 4, "嗯。好的，没问题。",
			[]string{"嗯。好的，没问题。"}},
		{"firstMinRunes=0 关闭提前切", 0, "我看到你坐在电脑前面，旁边有一个杯子。",
			[]string{"我看到你坐在电脑前面，旁边有一个杯子。"}},
		{"过长时在最后一个逗号处切", 4, "好的。" + thirty + "，" + fifteen + "。",
			[]string{"好的。", thirty + "，", fifteen + "。"}},
		{"去掉 Markdown 和表情", 4, "**好的**😀，我来看看✅。",
			[]string{"好的，我来看看。"}},
		{"没有边界时由 Flush 输出", 4, "我看到了一个杯子",
			[]string{"我看到了一个杯子"}},
		{"只有标点不输出", 4, "。", nil},
		{"小数点不是边界", 4, "大约3.5米。", []string{"大约3.5米。"}},
		{"保留要读的符号", 4, "现在25℃。", []string{"现在25℃。"}},
		{"日语按句号切", 4, "今日はいい天気ですね。明日は雨かもしれません。",
			[]string{"今日はいい天気ですね。", "明日は雨かもしれません。"}},
		{"英语：句点后跟下一句才切，小数点不切", 4, "Sure. It is about 3.5 meters, I think. Bye!",
			[]string{"Sure.", "It is about 3.5 meters, I think.", "Bye!"}},
		{"撇号、千分位逗号不是停顿处", 4, "Well I don't know, it costs 1,000 yen.",
			[]string{"Well I don't know,", "it costs 1,000 yen."}},
		{"英文按词数提前切（不按字母数）", 4, "Well, I think so, yes.",
			[]string{"Well, I think so,", "yes."}},
		{"英文过长时按词数在最后一个逗号处切", 4, "Okay. Then " + englishWords(29) + ", " + englishWords(15) + ".",
			[]string{"Okay.", "Then " + englishWords(29) + ",", englishWords(15) + "."}},
		{"省略号后面不急着切，攒够了在停顿处切", 4, "嗯……让我想想，好的。",
			[]string{"嗯……让我想想，", "好的。"}},
		{"分号不是句子边界，只是停顿处", 0, "第一点是价格；第二点是质量。",
			[]string{"第一点是价格；第二点是质量。"}},
	}
	// 同一段文字按不同粒度喂入（整段 / 逐字 / 每 3 个字），结果必须一致：模型的 token 边界不应影响切句。
	chunkings := map[string]func(string) []string{
		"整段":  func(s string) []string { return []string{s} },
		"逐字":  func(s string) []string { return chunkRunes(s, 1) },
		"每3字": func(s string) []string { return chunkRunes(s, 3) },
	}
	for _, tc := range cases {
		for chunkName, chunk := range chunkings {
			t.Run(tc.name+"/"+chunkName, func(t *testing.T) {
				segmenter := NewSentenceSegmenter(tc.firstMinRunes, 40)
				var got []string
				for _, piece := range chunk(tc.input) {
					got = append(got, segmenter.Push(piece)...)
				}
				got = append(got, segmenter.Flush()...)
				if !slices.Equal(got, tc.want) {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func chunkRunes(text string, size int) []string {
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += size {
		chunks = append(chunks, string(runes[start:min(start+size, len(runes))]))
	}
	return chunks
}

// 后面暂时没有字时：确定完了的句子马上交出去（调用工具前说的话不用等工具调用写完），英文句点要等下一个字（可能是小数点）。
func TestSentenceSegmenterEmitsCertainEndsRightAway(t *testing.T) {
	segmenter := NewSentenceSegmenter(4, 40)
	if got := segmenter.Push("好的，我查一下天气。"); !slices.Equal(got, []string{"好的，我查一下天气。"}) {
		t.Errorf("句号后面没有字也确定完了：应马上交出，得到 %q", got)
	}
	if got := segmenter.Push("It is about 3."); len(got) != 0 {
		t.Errorf("英文句点后面还没来字（可能是小数点）：应先等，得到 %q", got)
	}
	if got := append(segmenter.Push("5 meters long."), segmenter.Flush()...); !slices.Equal(got, []string{"It is about 3.5 meters long."}) {
		t.Errorf("小数不能切开：%q", got)
	}
}

// englishWords 是 n 个英文单词（用空格隔开）。
func englishWords(n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = "word"
	}
	return strings.Join(words, " ")
}
