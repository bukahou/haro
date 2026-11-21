package brain

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// 不经模型、直接念给主人的话：模型用不了时的说明（连不上、额度用完、账本读不出、模型拒答）。每种语言一份，
// 按主人最近一句话识别出的语种选；程序里带一份（phrases.json），部署时可以用文件换掉。

// 话的键（SpokenError.Phrase 交的就是它们）。话里的 {名字} 换成交过来的值。
const (
	PhraseOverMonthlyBudget = "over_monthly_budget" // {spent} {limit}：每月总费用到上限
	PhraseOverResearchDaily = "over_research_daily" // {spent} {limit}：今天的调查到上限
	PhraseOverSearchDaily   = "over_search_daily"   // {used} {limit}：今天的搜索到上限
	PhraseLedgerUnavailable = "ledger_unavailable"  // 读不出费用账本
	PhraseRefused           = "refused"             // 模型拒绝回答
	PhraseModelUnavailable  = "model_unavailable"   // {time}：连不上云端的模型（最低能力：说明状态，顺带报时）
	// {spent} {limit} {raise}：每月总费用到上限，可以口头临时提高 {raise} 日元
	PhraseOverMonthlyBudgetRaisable = "over_monthly_budget_raisable"
	PhraseConfirmCapRaise           = "confirm_cap_raise"  // {raise}：问主人要不要临时提高上限
	PhraseMonthlyCapRaised          = "monthly_cap_raised" // {limit}：上限提高了
)

var phraseKeys = []string{PhraseOverMonthlyBudget, PhraseOverResearchDaily, PhraseOverSearchDaily,
	PhraseLedgerUnavailable, PhraseRefused, PhraseModelUnavailable, PhraseOverMonthlyBudgetRaisable, PhraseConfirmCapRaise,
	PhraseMonthlyCapRaised}

// Phrases 是每种语言的固定的话：语种（识别给的，例如 zh、ja、en）→ 键 → 话。Default 是没有这种语言时用的。
type Phrases struct {
	Default   string                       `json:"default"`
	Languages map[string]map[string]string `json:"languages"`
}

//go:embed phrases.json
var builtinPhrases []byte

// LoadPhrases 读固定的话：path 为空用程序里带的那份。每种语言都要有全部的键，默认语言要在里面。
func LoadPhrases(path string) (Phrases, error) {
	data := builtinPhrases
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return Phrases{}, fmt.Errorf("读取固定的话: %w", err)
		}
	}
	var phrases Phrases
	if err := json.Unmarshal(data, &phrases); err != nil {
		return Phrases{}, fmt.Errorf("解析固定的话 %s: %w", path, err)
	}
	if _, ok := phrases.Languages[phrases.Default]; !ok {
		return Phrases{}, fmt.Errorf("固定的话 %s：默认语言 %q 不在 languages 里", path, phrases.Default)
	}
	for language, texts := range phrases.Languages {
		for _, key := range phraseKeys {
			if strings.TrimSpace(texts[key]) == "" {
				return Phrases{}, fmt.Errorf("固定的话 %s：%s 缺少 %s", path, language, key)
			}
		}
	}
	return phrases, nil
}

// builtin 是程序里带的那份（测试、没配置时用）；它读不出来就是程序写错了。
func builtinPhrasesOrPanic() Phrases {
	phrases, err := LoadPhrases("")
	if err != nil {
		panic(err)
	}
	return phrases
}

// Say 取 language 的 key 那句，填上 values；没有这种语言用默认语言。
func (p Phrases) Say(language, key string, values map[string]string) string {
	texts, ok := p.Languages[language]
	if !ok {
		texts = p.Languages[p.Default]
	}
	text := texts[key]
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		text = strings.ReplaceAll(text, "{"+name+"}", values[name])
	}
	return text
}
