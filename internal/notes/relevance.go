package notes

import (
	"slices"
	"strings"
	"unicode"
)

// RankByRelevance 按中文两字片段（bigram）的重合度给候选记录打分，返回最相关的 limit 条（分数为 0 的不要）。
// candidates 应按时间倒序传入：同分时保持原顺序，即新的在前。
//
// 与具体数据库无关，各 Repository 实现共用。为什么不用 LIKE：模型给的查询词往往是问句的改写
// （「车停在哪」），和原文（「车停在C3的7号位」）字面不同（2026-11-15 实测）。
// 以后升级向量检索时，换掉的就是这一层。
func RankByRelevance(candidates []Note, query string, limit int) []Note {
	queryGrams := bigrams(query)
	type scored struct {
		note  Note
		score int
	}
	var matches []scored
	for _, note := range candidates {
		if score := overlap(queryGrams, bigrams(note.Content)); score > 0 {
			matches = append(matches, scored{note, score})
		}
	}
	slices.SortStableFunc(matches, func(a, b scored) int { return b.score - a.score })
	ranked := make([]Note, 0, min(limit, len(matches)))
	for _, match := range matches[:min(limit, len(matches))] {
		ranked = append(ranked, match.note)
	}
	return ranked
}

// stopGrams 是问句里常见、却不代表内容的两字片段，不参与打分。
var stopGrams = map[string]bool{"在哪": true, "哪了": true, "哪里": true, "什么": true, "是什": true, "多少": true,
	"我的": true, "一下": true, "记得": true, "之前": true, "上次": true}

// bigrams 把文字切成两字片段（只取字母、数字、汉字；英文和数字按整词算一个片段）。
func bigrams(text string) map[string]bool {
	grams := map[string]bool{}
	var hanRun []rune
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			grams[strings.ToLower(word.String())] = true
			word.Reset()
		}
	}
	flushHan := func() {
		for i := 0; i+1 < len(hanRun); i++ {
			if gram := string(hanRun[i : i+2]); !stopGrams[gram] {
				grams[gram] = true
			}
		}
		hanRun = hanRun[:0]
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			hanRun = append(hanRun, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word.WriteRune(r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return grams
}

func overlap(a, b map[string]bool) int {
	count := 0
	for gram := range a {
		if b[gram] {
			count++
		}
	}
	return count
}
