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
	// {title} {minutes}：日历里的日程提前 {minutes} 分钟在终端响的提醒（提前 0 分钟的直接念标题）
	PhraseCalendarAlertAhead = "calendar_alert_ahead"
)

var phraseKeys = []string{PhraseOverMonthlyBudget, PhraseOverResearchDaily, PhraseOverSearchDaily,
	PhraseLedgerUnavailable, PhraseRefused, PhraseModelUnavailable, PhraseOverMonthlyBudgetRaisable, PhraseConfirmCapRaise,
	PhraseMonthlyCapRaised, PhraseCalendarAlertAhead}

// Phrases 是每种语言的固定的话：语种（识别给的，例如 zh、ja、en）→ 键 → 话。Default 是没有这种语言时用的。
type Phrases struct {
	Default   string                       `json:"default"`
	Languages map[string]map[string]string `json:"languages"`
	// Money 是每种语言里金额怎么念：币种代码 → 写法（{amount} 换成数值）；"*" 是没列出的币种（{currency} 换成代码）。
	// 话里写 {名字:money} 的地方按它念，币种取 values 里的 PhraseCurrency。
	Money map[string]map[string]string `json:"money"`
}

// PhraseCurrency 是 values 里放币种代码（ISO 4217）的名字：话里 {名字:money} 的金额都按这个币种念。
const PhraseCurrency = "currency"

// moneyAnyCurrency 是 Money 里「没列出的币种」那一项。
const moneyAnyCurrency = "*"

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
		if !strings.Contains(phrases.Money[language][moneyAnyCurrency], "{amount}") {
			return Phrases{}, fmt.Errorf("固定的话 %s：money 里 %s 缺少 %q（没列出的币种怎么念，要有 {amount}）", path, language, moneyAnyCurrency)
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
	if _, ok := p.Languages[language]; !ok {
		language = p.Default
	}
	text := p.moneyPlaceholders(language, p.Languages[language][key], values)
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

// moneyPlaceholders 把话里的 {名字:money} 换成按这种语言念的金额（数值取 values[名字]，币种取 values[PhraseCurrency]）。
func (p Phrases) moneyPlaceholders(language, text string, values map[string]string) string {
	formats := p.Money[language]
	code := values[PhraseCurrency]
	format, ok := formats[code]
	if !ok {
		format = strings.ReplaceAll(formats[moneyAnyCurrency], "{currency}", code)
	}
	for name, value := range values {
		text = strings.ReplaceAll(text, "{"+name+":money}", strings.ReplaceAll(format, "{amount}", value))
	}
	return text
}
