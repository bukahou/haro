package profile

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// warnings 数警告级的日志（按级别数，不比对文字）。
type warnings struct {
	mu    sync.Mutex
	count int
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnings) WithGroup(string) slog.Handler            { return w }
func (w *warnings) Handle(_ context.Context, record slog.Record) error {
	if record.Level == slog.LevelWarn {
		w.mu.Lock()
		w.count++
		w.mu.Unlock()
	}
	return nil
}

func (w *warnings) seen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// front matter 按 YAML 读：person 写成列表、行尾带注释都认；以前用逗号分开写几个名字的也认；
// 写了不认识的项（拼错了）这份资料不用（记警告），不会悄悄当成别人的。
func TestProfileFrontMatterIsYAML(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "owner.md", "---\nperson: [主人, 太太]   # 两个人共用一份\n---\n- 喜欢的：咖啡\n")
	write(t, dir, "guest.md", "---\nperson: 客人 # 注释不算名字\n---\n- 喜欢的：茶\n")
	write(t, dir, "legacy.md", "---\nperson: 老人, 小孩\n---\n- 喜欢的：散步\n")
	write(t, dir, "typo.md", "---\nperson: 邻居\npreson_note: 拼错的项\n---\n- 喜欢的：猫\n")
	log := &warnings{}
	store := OpenDir(dir, slog.New(log))
	for person, want := range map[string]string{"主人": "咖啡", "太太": "咖啡", "客人": "茶", "老人": "散步", "小孩": "散步"} {
		if got := store.Profile(person); !strings.Contains(got, want) {
			t.Errorf("%s 的资料应有 %q：%q", person, want, got)
		}
	}
	if _, people := store.Summary(); len(people) != 5 {
		t.Errorf("名字应是 5 个（不含写错的那份）：%v", people)
	}
	if got := store.Profile("邻居"); strings.Contains(got, "猫") || log.seen() == 0 {
		t.Errorf("写了不认识的项：这份不用，并记警告：%q（警告 %d 条）", got, log.seen())
	}
}

// 资料目录读出错（不是不存在，例如挂载出错时那个路径不是目录）：不当成没有资料，先用上次读到的，同一段出错只记一次警告；
// 目录真的不存在了才当成没有资料。
func TestProfileKeepsLastReadOnReadError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profile")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n- 怎么称呼：小王\n")
	log := &warnings{}
	store := OpenDir(dir, slog.New(log))
	if got := store.Profile("主人"); !strings.Contains(got, "小王") {
		t.Fatalf("先读到资料：%q", got)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("不是目录"), 0o600); err != nil { // 读这个「目录」会出错，但它不是「不存在」
		t.Fatal(err)
	}
	for range 2 {
		if got := store.Profile("主人"); !strings.Contains(got, "小王") {
			t.Errorf("读目录出错：应先用上次读到的：%q", got)
		}
	}
	if log.seen() != 1 {
		t.Errorf("同一段出错只记一次警告：%d 条", log.seen())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if got := store.Profile("主人"); got != "" {
		t.Errorf("目录不存在了：当成没有资料：%q", got)
	}
}

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

// 列表里冒号结尾、下面有填了的缩进子项的是小标题：保留（子项才有归属）；没有子项的空项照样不发。
func TestProfileKeepsHeadingsWithSubItems(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n- 称呼：\n- 兴趣：\n  - 登山\n  - 摄影\n- 工作：工程师\n- 小时候：\n  - \n")
	got := store(t, dir).Profile("主人")
	if !strings.Contains(got, "- 兴趣：\n  - 登山\n  - 摄影") {
		t.Errorf("带子项的小标题应保留：%q", got)
	}
	if strings.Contains(got, "称呼") || strings.Contains(got, "小时候") {
		t.Errorf("没填的项（包括子项都没填的）不该发：%q", got)
	}
}

// 超长时截在完整的一行末尾，不切断半句。
func TestProfileCutsAtLineEnd(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("长", 100) + "。"
	write(t, dir, "owner.md", "---\nperson: 主人\n---\n"+strings.Repeat(line+"\n", 15))
	got := store(t, dir).Profile("主人")
	lines := 0
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "长") {
			lines++
			if l != line {
				t.Fatalf("截在了半句：%q", l)
			}
		}
	}
	if want := personAdviseRunes * hardLimitFactor / len([]rune(line+"\n")); lines != want {
		t.Errorf("应保留 %d 行完整的，得到 %d 行", want, lines)
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
	if names, _, err := parse(data); err != nil || len(names) != 1 || names[0] != "主人" {
		t.Errorf("模板的 person：%v", names)
	}
}
