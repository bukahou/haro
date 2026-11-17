package brain

import "testing"

func TestTakeEmotion(t *testing.T) {
	cases := []struct {
		sentence string
		current  Emotion
		text     string
		emotion  Emotion
	}{
		{"[调侃]你问我冰箱里有什么？", EmotionDefault, "你问我冰箱里有什么？", EmotionTease},
		{"我又不是冰箱。", EmotionTease, "我又不是冰箱。", EmotionTease},            // 没有标记：沿用上一句
		{"【笑】哈哈哈，好问题。", EmotionDefault, "哈哈哈，好问题。", EmotionLaugh},     // 全角括号
		{"[ 轻声 ]嘘——小声点。", EmotionDefault, "嘘——小声点。", EmotionWhisper},  // 括号里有空白
		{"[平常]现在三点。", EmotionTease, "现在三点。", EmotionDefault},          // 显式回到平常
		{"[激动]什么？[温柔]没事的。", EmotionDefault, "什么？没事的。", EmotionGentle}, // 多个标记：最后一个为准
		{"[撩]今天这身打扮不错嘛。", EmotionDefault, "今天这身打扮不错嘛。", EmotionFlirt},
		{"[笑]", EmotionDefault, "", EmotionLaugh},                      // 只有标记：文字为空，情绪留给下一句
		{"车停在 [B2] 层。", EmotionDefault, "车停在 [B2] 层。", EmotionDefault}, // 不认识的方括号原样保留
	}
	for _, c := range cases {
		text, emotion := takeEmotion(c.sentence, c.current)
		if text != c.text || emotion != c.emotion {
			t.Errorf("takeEmotion(%q, %q) = %q, %q；应为 %q, %q", c.sentence, c.current, text, emotion, c.text, c.emotion)
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	cases := map[string]Language{
		"现在是下午三点。":            LanguageChinese,
		"你的 iPhone 电量只剩 15%。": LanguageChinese,
		"えっ、また寝坊したの？":         LanguageJapanese,
		"コーヒーを飲む。":            LanguageJapanese, // 只有片假名和汉字
		"東京タワー":               LanguageJapanese,
		"好的・明白":               LanguageChinese, // 中点不算假名
	}
	for text, want := range cases {
		if got := detectLanguage(text); got != want {
			t.Errorf("detectLanguage(%q) = %q，应为 %q", text, got, want)
		}
	}
}
