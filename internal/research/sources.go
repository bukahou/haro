package research

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// 调查时按网址记下拿到的资料：审查时从这里取草稿列出的那几个来源的摘录（不给调查过程）。读到的正文优先，没读到的用搜索结果的
// 标题和摘要。

const (
	// maxExcerptRunes 是给审查的每个来源最多多少字：和调查时每页给模型的一样多（pageRunes），审查看到的就是调查时看到的
	// （资料给少了会把真话判成没依据：对比试验里摘录截短后删掉过真信息）。搜索结果的摘要一般几百字，碰不到它。
	maxExcerptRunes = pageRunes
	// maxReviewSourceRunes 是给审查的摘录一共最多多少字：草稿按要求列 1 到 3 个来源，三份整页的摘录放得下；
	// 多列的来源放不下就写明没有附上。也管住审查一次的输入（约几千 token）。
	maxReviewSourceRunes = 3 * pageRunes
)

// 给审查的摘录里的说明（取自哪里、为什么没有）。
const (
	fromPage       = "网页正文（调查时读到的）"
	fromSnippet    = "搜索结果的标题和摘要（调查时没读正文）"
	noteMissing    = "调查时没有拿到这个来源的内容"
	noteOmitted    = "超出给审查的总长，没有附上"
	noteTruncation = "太长，后面没有附上"
)

// sourceNotes 是按网址记下的资料（键是 sourceKey）。
type sourceNotes map[string]sourceNote

type sourceNote struct {
	title, snippet string // 搜索结果里的
	content        string // 读到的正文（已截到每页的上限）
}

// addHits 记下搜索结果：同一个网址的标题、摘要留第一次的（读到的正文另记，优先用）。
func (n sourceNotes) addHits(hits []SearchHit) {
	for _, hit := range hits {
		key := sourceKey(hit.URL)
		if key == "" {
			continue
		}
		note := n[key]
		if note.title == "" {
			note.title = hit.Title
		}
		if note.snippet == "" {
			note.snippet = hit.Snippet
		}
		n[key] = note
	}
}

// addPages 记下读到的正文（没读到的那页不记）。
func (n sourceNotes) addPages(pages []Page) {
	for _, page := range pages {
		key := sourceKey(page.URL)
		if key == "" || page.Error != "" || strings.TrimSpace(page.Content) == "" {
			continue
		}
		note := n[key]
		note.content = page.Content
		n[key] = note
	}
}

// sourceExcerpt 是草稿列出的一个来源给审查看的摘录。
type sourceExcerpt struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	From    string `json:"from,omitempty"` // 摘录取自哪里
	Excerpt string `json:"excerpt,omitempty"`
	Note    string `json:"note,omitempty"` // 没拿到内容、超出总长没附上、截短了
}

// excerptsFor 是草稿列出的来源的摘录（按草稿列的顺序，同一个网址只给一次）：每个最多 maxExcerptRunes 字，一共最多
// maxReviewSourceRunes 字；调查时没拿到内容的、放不下的写明。
func (n sourceNotes) excerptsFor(urls []string) []sourceExcerpt {
	excerpts := []sourceExcerpt{}
	seen := map[string]bool{}
	remaining := maxReviewSourceRunes
	for _, raw := range urls {
		key := sourceKey(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		excerpt := sourceExcerpt{URL: strings.TrimSpace(raw)}
		note := n[key]
		text, from := note.content, fromPage
		if text == "" {
			text, from = note.snippet, fromSnippet
		}
		switch {
		case text == "":
			excerpt.Note = noteMissing
		case remaining <= 0:
			excerpt.Note = noteOmitted
		default:
			var cut bool
			excerpt.Title, excerpt.From = note.title, from
			excerpt.Excerpt, cut = truncateRunes(text, min(maxExcerptRunes, remaining))
			if cut {
				excerpt.Note = noteTruncation
			}
			remaining -= utf8.RuneCountInString(excerpt.Excerpt)
		}
		excerpts = append(excerpts, excerpt)
	}
	return excerpts
}

// sourceKey 是网址拿来对照的写法：按结构解析后协议、主机名不分大小写，去掉 # 后面的部分和路径末尾的 /（模型抄网址时常差在这些
// 地方）；解析不出主机名的照原样（去掉首尾空白）。
func sourceKey(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	parsed.Fragment, parsed.RawFragment = "", ""
	parsed.Path, parsed.RawPath = strings.TrimSuffix(parsed.Path, "/"), strings.TrimSuffix(parsed.RawPath, "/")
	return parsed.String()
}

// truncateRunes 截到 limit 个字（按字符，不切坏中文）；返回有没有截。
func truncateRunes(text string, limit int) (string, bool) {
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	return string([]rune(text)[:limit]), true
}
