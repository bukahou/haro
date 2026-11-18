package brain

import "strings"

// correctionPhrases 是主人在纠正上一句时常说的话（中文为主；识别有时会出日语）。
// 命中时这一轮标上 Escalate，交给更强的模型重新回答 —— 规则写在代码里，不让模型自己判断要不要换。
var correctionPhrases = []string{
	"不对", "错了", "不是这个", "不是这样", "说错", "搞错", "弄错", "再想想", "再看看", "重新看", "重新想",
	"違う", "ちがう", "間違",
}

// notCorrections 是含有纠正用词、但不是在纠正的说法（先去掉再判断）。
var notCorrections = []string{"对不对", "不对劲", "没错了", "错了吗"}

// isCorrection 判断主人这句话是不是在纠正上一句。
func isCorrection(text string) bool {
	for _, phrase := range notCorrections {
		text = strings.ReplaceAll(text, phrase, "")
	}
	for _, phrase := range correctionPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
