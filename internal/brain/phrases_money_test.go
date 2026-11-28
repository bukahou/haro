package brain_test

import (
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// 话里的 {名字:money} 按这种语言念币种（写在固定的话的文件里）；没列出的币种念「数值 代码」。
func TestPhrasesSayMoney(t *testing.T) {
	phrases, err := brain.LoadPhrases("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		language, currency, want string
	}{
		{"zh", "JPY", "要把这个月的上限临时提高 500 日元吗？"},
		{"en", "USD", "Do you want to raise this month's limit by 500 dollars for now?"},
		{"ja", "CNY", "今月の上限を一時的に 500 人民元上げますか？"},
		{"zh", "EUR", "要把这个月的上限临时提高 500 EUR吗？"},
	} {
		got := phrases.Say(c.language, brain.PhraseConfirmCapRaise, map[string]string{"raise": "500", brain.PhraseCurrency: c.currency})
		if got != c.want {
			t.Errorf("%s / %s：%q，应是 %q", c.language, c.currency, got, c.want)
		}
	}
}
