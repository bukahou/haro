package voiceprint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadMatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "voiceprints")
	owner := Voiceprint{Name: "主人", Model: "campplus.onnx", EnrolledAt: time.Now(),
		Embeddings: [][]float32{{1, 0, 0}, {0.9, 0.1, 0}}}
	path, err := Save(dir, owner)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("声纹文件权限应为 0600，是 %v", info.Mode().Perm())
	}
	_, _ = Save(dir, Voiceprint{Name: "旧模型", Model: "old.onnx", Embeddings: [][]float32{{0, 1, 0}}})

	loaded, err := LoadAll(dir, "campplus.onnx")
	if err != nil || len(loaded) != 1 || loaded[0].Name != "主人" {
		t.Fatalf("应只读到同一模型的声纹：%+v %v", loaded, err)
	}
	if name, score := Match(loaded, []float32{0.95, 0.05, 0}, 0.5); name != "主人" || score < 0.99 {
		t.Errorf("像的应认出来：%q %.3f", name, score)
	}
	if name, score := Match(loaded, []float32{0, 0, 1}, 0.5); name != "" || score > 0.01 {
		t.Errorf("不像的不应认出来：%q %.3f", name, score)
	}
	if empty, err := LoadAll(filepath.Join(dir, "不存在"), "campplus.onnx"); err != nil || len(empty) != 0 {
		t.Errorf("没有目录 = 没人登记：%v %v", empty, err)
	}
}

func TestSaveRejectsBadName(t *testing.T) {
	for _, name := range []string{"", "../etc", "a/b", "名 字"} {
		if _, err := Save(t.TempDir(), Voiceprint{Name: name, Embeddings: [][]float32{{1}}}); err == nil {
			t.Errorf("名字 %q 应被拒绝", name)
		}
	}
}

func TestLoadOne(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir, "主人"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("没登记过应返回 ErrNotExist：%v", err)
	}
	_, _ = Save(dir, Voiceprint{Name: "主人", Model: "m", Embeddings: [][]float32{{1, 0}}})
	if v, err := Load(dir, "主人"); err != nil || len(v.Embeddings) != 1 {
		t.Errorf("读回 %+v %v", v, err)
	}
}
