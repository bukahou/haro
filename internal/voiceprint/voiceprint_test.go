package voiceprint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
		if _, err := Save(t.TempDir(), Voiceprint{Name: name, Embeddings: [][]float32{{1}}}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("名字 %q 应被拒绝（ErrInvalidName）：%v", name, err)
		}
		if _, err := Load(t.TempDir(), name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("读名字 %q 也应拒绝（ErrInvalidName）：%v", name, err)
		}
	}
}

// 同时存同一个人：每次都成功（临时文件各用各的），存完的文件是完整的某一次，不留临时文件。
func TestSaveConcurrently(t *testing.T) {
	dir := t.TempDir()
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			embeddings := make([][]float32, i+1) // 第 i 次存 i+1 句：读回来能看出是完整的哪一次
			for j := range embeddings {
				embeddings[j] = []float32{float32(i), 1}
			}
			_, err := Save(dir, Voiceprint{Name: "主人", Model: "m", Embeddings: embeddings})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("同时存同一个人不应失败：%v", err)
		}
	}
	saved, err := Load(dir, "主人")
	if err != nil {
		t.Fatalf("存完应读得出来：%v", err)
	}
	for _, embedding := range saved.Embeddings {
		if int(embedding[0]) != len(saved.Embeddings)-1 {
			t.Fatalf("读回来的应是完整的某一次（%d 句），混进了别的：%v", len(saved.Embeddings), saved.Embeddings)
		}
	}
	requireNoTempFiles(t, dir)
}

// 存盘失败（这里是目标位置被一个目录占着，改名不过去）：报错，并删掉临时文件。
func TestSaveFailureRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "主人.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(dir, Voiceprint{Name: "主人", Model: "m", Embeddings: [][]float32{{1}}}); err == nil {
		t.Fatal("改名失败应报错")
	}
	requireNoTempFiles(t, dir)
}

func requireNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) > 0 {
		t.Errorf("不应留下临时文件：%v", leftovers)
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
