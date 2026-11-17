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
