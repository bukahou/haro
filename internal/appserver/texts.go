package appserver

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/text/language"
)

// App 上给人看的文字（用途的名字、换算提示、出错的说明）：每种语言一份，按请求的 Accept-Language 选；⛔ 不写在代码里。
//
//go:embed texts.json
var builtinTexts []byte

type texts struct {
	Default   string                       `json:"default"`
	Languages map[string]map[string]string `json:"languages"`
	matcher   language.Matcher
	tags      []string
}

func loadTexts() (*texts, error) {
	var loaded texts
	if err := json.Unmarshal(builtinTexts, &loaded); err != nil {
		return nil, fmt.Errorf("App 的文字: %w", err)
	}
	if _, ok := loaded.Languages[loaded.Default]; !ok {
		return nil, fmt.Errorf("App 的文字：默认语言 %q 不在里面", loaded.Default)
	}
	defaults := loaded.Languages[loaded.Default]
	supported := []language.Tag{language.Make(loaded.Default)} // 第一个是匹配不上时用的
	loaded.tags = []string{loaded.Default}
	for code, entries := range loaded.Languages {
		for key := range defaults {
			if strings.TrimSpace(entries[key]) == "" {
				return nil, fmt.Errorf("App 的文字：%s 缺少 %s", code, key)
			}
		}
		if code != loaded.Default {
			supported = append(supported, language.Make(code))
			loaded.tags = append(loaded.tags, code)
		}
	}
	loaded.matcher = language.NewMatcher(supported)
	return &loaded, nil
}

// pick 按 Accept-Language 选一种有的语言（没带、都不支持时用默认语言）。
func (t *texts) pick(acceptLanguage string) string {
	wanted, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(wanted) == 0 {
		return t.Default
	}
	_, index, _ := t.matcher.Match(wanted...)
	return t.tags[index]
}

// say 取 lang 的 key 那句，填上 values（{名字} 换成值）。
func (t *texts) say(lang, key string, values map[string]string) string {
	text := t.Languages[lang][key]
	if text == "" {
		text = t.Languages[t.Default][key]
	}
	for name, value := range values {
		text = strings.ReplaceAll(text, "{"+name+"}", value)
	}
	return text
}
