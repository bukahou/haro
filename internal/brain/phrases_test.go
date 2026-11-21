package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 程序里带的那份：每种语言都有全部的键；配置的文件缺了哪种语言的哪一句，启动时就报出来。
func TestLoadPhrasesChecksEveryKey(t *testing.T) {
	builtin, err := LoadPhrases("")
	if err != nil {
		t.Fatalf("程序里带的那份读不出来：%v", err)
	}
	for _, language := range []string{"zh", "ja", "en"} {
		if text := builtin.Say(language, PhraseRefused, nil); text == "" {
			t.Errorf("%s 没有 %s", language, PhraseRefused)
		}
	}
	path := filepath.Join(t.TempDir(), "phrases.json")
	broken := `{"default":"zh","languages":{"zh":{"refused":"答不了。"}}}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPhrases(path); err == nil || !strings.Contains(err.Error(), PhraseOverMonthlyBudget) {
		t.Errorf("缺了键应报出是哪一句：%v", err)
	}
}

// 填值；没有这种语言时用默认语言。
func TestPhrasesSay(t *testing.T) {
	phrases := Phrases{Default: "zh", Languages: map[string]map[string]string{
		"zh": {"k": "用了 {used} 次，上限 {limit} 次。"}, "en": {"k": "Used {used} of {limit}."}}}
	values := map[string]string{"used": "30", "limit": "30"}
	if got := phrases.Say("en", "k", values); got != "Used 30 of 30." {
		t.Errorf("英语：%q", got)
	}
	if got := phrases.Say("fr", "k", values); got != "用了 30 次，上限 30 次。" {
		t.Errorf("没有法语：用默认语言：%q", got)
	}
}
