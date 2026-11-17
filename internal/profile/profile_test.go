package profile

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfilePerPerson(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, HouseholdFile, "<!-- 说明不发给模型 -->\n## 家里\n- 家里有谁：我和猫\n- 宠物：\n")
	write(t, dir, "owner.md", "---\nperson: 主人, 老大\n---\n## 偏好\n- 不喜欢的：香菜\n- 兴趣：\n")
	write(t, dir, "guest.md", "## 没写 person 的文件会被跳过\n- 内容：有\n")
	write(t, dir, "README.md", "说明文件不算资料")
	store := OpenDir(dir, quietLogger)

	owner := store.Profile("主人")
	for _, want := range []string{"主人资料", "〈全家〉", "家里有谁：我和猫", "〈主人〉", "不喜欢的：香菜"} {
		if !strings.Contains(owner, want) {
			t.Errorf("主人的资料里应有 %q：\n%s", want, owner)
		}
	}
	for _, unwanted := range []string{"说明不发给模型", "宠物：", "兴趣：", "没写 person", "说明文件"} {
		if strings.Contains(owner, unwanted) {
			t.Errorf("不该出现 %q：\n%s", unwanted, owner)
		}
	}
	if alias := store.Profile("老大"); !strings.Contains(alias, "香菜") {
		t.Errorf("一个人可以有几个名字：%s", alias)
	}
	shared := store.Profile("_shared")
	if !strings.Contains(shared, "我和猫") || strings.Contains(shared, "香菜") {
		t.Errorf("认不出是谁时只给全家的：%s", shared)
	}
	if household, people := store.Summary(); household == 0 || len(people) != 2 {
		t.Errorf("概况：%d %v", household, people)
	}
}

func TestProfileReloadsWhenFilesChange(t *testing.T) {
	dir := t.TempDir()
	store := OpenDir(dir, quietLogger)
	if got := store.Profile("主人"); got != "" {
		t.Fatalf("还没有资料：%q", got)
	}
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n- 怎么称呼：小王\n")
	if got := store.Profile("主人"); !strings.Contains(got, "小王") {
		t.Fatalf("新建的文件下一轮就生效：%q", got)
	}
	time.Sleep(10 * time.Millisecond) // 让修改时间不同
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n- 怎么称呼：老王（改了）\n")
	if got := store.Profile("主人"); !strings.Contains(got, "老王（改了）") {
		t.Fatalf("改了文件下一轮就生效：%q", got)
	}
	os.Remove(filepath.Join(dir, "owner.md"))
	if got := store.Profile("主人"); got != "" {
		t.Fatalf("删了文件就没有了：%q", got)
	}
}

func TestProfileLengthLimit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n"+strings.Repeat("长", personAdviseRunes*hardLimitFactor+100))
	got := store(t, dir).Profile("主人")
	if n := strings.Count(got, "长"); n != personAdviseRunes*hardLimitFactor {
		t.Errorf("超过两倍建议字数的部分截掉：%d 字", n)
	}
}

func store(t *testing.T, dir string) *Store {
	t.Helper()
	return OpenDir(dir, quietLogger)
}

// 公开仓库里的模板：原样（只有标题和空项）时什么都不发给模型；front matter 写对了。
func TestExampleTemplatesAreEmpty(t *testing.T) {
	dir := filepath.Join("..", "..", "assets", "profile.example")
	templates := OpenDir(dir, quietLogger)
	if got := templates.Profile("主人"); got != "" {
		t.Errorf("没填的模板不该发给模型：%q", got)
	}
	if household, people := templates.Summary(); household != 0 || len(people) != 0 {
		t.Errorf("没填的模板不算有资料：%d %v", household, people)
	}
	data, err := os.ReadFile(filepath.Join(dir, "owner.md"))
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := parse(string(data)); len(names) != 1 || names[0] != "主人" {
		t.Errorf("模板的 person：%v", names)
	}
}
