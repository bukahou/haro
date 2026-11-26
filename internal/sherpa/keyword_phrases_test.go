package sherpa

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestKeywordPhrases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keywords.txt")
	content := "# 注释行\n" +
		"h ā l uó h ā l uó :2.0 @哈罗哈罗\n" +
		"\n" +
		"n ǐ h ǎo #0.3 :1.5\n" + // 没写显示名：用拼音单元
		"h ā l uó h ā l uó @哈罗哈罗\n" // 重复的显示名只算一次
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := KeywordPhrases(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"哈罗哈罗", "n ǐ h ǎo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("唤醒词 = %q，应为 %q", got, want)
	}
}

// 仓库里带的关键词文件读得出唤醒词（镜像里用的就是它）。
func TestKeywordPhrasesOfShippedFile(t *testing.T) {
	got, err := KeywordPhrases(filepath.Join("..", "..", "assets", "wake-words.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] == "" {
		t.Fatalf("仓库里的关键词文件应至少有一个唤醒词，得到 %q", got)
	}
}

func TestKeywordPhrasesRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keywords.txt")
	if err := os.WriteFile(path, []byte("# 只有注释\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := KeywordPhrases(path); err == nil {
		t.Fatal("没有唤醒词的关键词文件应报错")
	}
}
