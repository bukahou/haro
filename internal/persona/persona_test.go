package persona

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

func TestLoadCatalogFromRepo(t *testing.T) {
	personas, err := LoadCatalog(filepath.Join("..", "..", "assets", "personas"))
	if err != nil {
		t.Fatal(err)
	}
	if len(personas) != 2 || personas[0].ID != "buddy" || personas[1].ID != "gentle" {
		t.Fatalf("应按文件名顺序读到仓库里的两个示例性格：%+v", personas)
	}
	if personas[0].DefaultEmotion != brain.EmotionTease || personas[1].DefaultEmotion != brain.EmotionGentle {
		t.Errorf("默认语气不对：%q / %q", personas[0].DefaultEmotion, personas[1].DefaultEmotion)
	}
}

func TestLoadCatalogRejectsBadFiles(t *testing.T) {
	write := func(dir, name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	write(dir, "a.json", `{"id":"a","name":"甲","prompt":"x","default_emotion":"angry"}`)
	if _, err := LoadCatalog(dir); err == nil {
		t.Error("不认识的情绪应报错")
	}
	dir = t.TempDir()
	write(dir, "a.json", `{"id":"a","name":"甲","prompt":"x"}`)
	write(dir, "b.json", `{"id":"a","name":"乙","prompt":"y"}`)
	if _, err := LoadCatalog(dir); err == nil {
		t.Error("ID 重复应报错")
	}
	if _, err := LoadCatalog(t.TempDir()); err == nil {
		t.Error("空目录应报错")
	}
}

func TestFileState(t *testing.T) {
	state := NewFileState(filepath.Join(t.TempDir(), "sub", "persona"))
	if id, err := state.LoadPersona(); err != nil || id != "" {
		t.Fatalf("没记过时应返回空：%q %v", id, err)
	}
	if err := state.SavePersona("gentle"); err != nil {
		t.Fatal(err)
	}
	if id, err := state.LoadPersona(); err != nil || id != "gentle" {
		t.Errorf("读回 %q %v", id, err)
	}
}
