package brain

import (
	"fmt"
	"github.com/bukahou/haro/internal/agents"
	"slices"
	"strings"
	"testing"
)

// 情绪标记是固定语法：认出来的变成「换语气」，文字里不留标记；没写完的标记先留着等后面的字；不认识的方括号原样保留。
// 同一段文字不管怎么分块喂进来，认出来的都一样。
func TestEmotionStream(t *testing.T) {
	cases := []struct {
		name, input string
		want        []emotionPiece
	}{
		{"句首标记", "[调侃]你问我冰箱里有什么？", []emotionPiece{{tag: true, emotion: EmotionTease}, {text: "你问我冰箱里有什么？"}}},
		{"全角括号、括号里有空白", "【笑】哈哈。[ 轻声 ]嘘——",
			[]emotionPiece{{tag: true, emotion: EmotionLaugh}, {text: "哈哈。"}, {tag: true, emotion: EmotionWhisper}, {text: "嘘——"}}},
		{"显式回到平常", "[平常]现在三点。", []emotionPiece{{tag: true, emotion: EmotionDefault}, {text: "现在三点。"}}},
		{"句号后面紧跟标记", "什么？[温柔]没事的。",
			[]emotionPiece{{text: "什么？"}, {tag: true, emotion: EmotionGentle}, {text: "没事的。"}}},
		{"不认识的方括号原样保留", "车停在 [B2] 层。", []emotionPiece{{text: "车停在 [B2] 层。"}}},
		{"没写完的标记收尾时当文字", "好的[笑", []emotionPiece{{text: "好的[笑"}}},
	}
	for _, c := range cases {
		for _, size := range []int{0, 1, 2} { // 0 = 整段
			t.Run(fmt.Sprintf("%s/%d", c.name, size), func(t *testing.T) {
				stream := &emotionStream{}
				var got []emotionPiece
				for _, piece := range chunked(c.input, size) {
					got = append(got, stream.Push(piece)...)
				}
				got = mergeText(append(got, stream.Flush()...))
				if !slices.Equal(got, c.want) {
					t.Errorf("got %+v, want %+v", got, c.want)
				}
			})
		}
	}
}

// chunked 按 size 个字分块（0 = 整段）。
func chunked(text string, size int) []string {
	if size == 0 {
		return []string{text}
	}
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += size {
		chunks = append(chunks, string(runes[start:min(start+size, len(runes))]))
	}
	return chunks
}

// mergeText 把相邻的文字片段并起来（分块喂入时文字会分成几片，内容一样）。
func mergeText(pieces []emotionPiece) []emotionPiece {
	var merged []emotionPiece
	for _, piece := range pieces {
		if n := len(merged); n > 0 && !piece.tag && !merged[n-1].tag {
			merged[n-1].text += piece.text
			continue
		}
		merged = append(merged, piece)
	}
	return merged
}

// 提示词里列出的标记来自同一份清单：前台的定义引用 {{.EmotionTags}}，值由 PromptValues 给。
func TestEmotionTagListComesFromTheTags(t *testing.T) {
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	front, _ := catalog.Get(agents.Front)
	prompt, err := front.RenderPrompt(PromptValues())
	if err != nil {
		t.Fatal(err)
	}
	list := emotionTagList()
	for _, tag := range emotionTags {
		if !strings.Contains(list, "["+tag.Label+"]") || !strings.Contains(prompt, "["+tag.Label+"]") {
			t.Errorf("标记 %s 应出现在清单和前台的提示词里：%s", tag.Label, list)
		}
	}
}
