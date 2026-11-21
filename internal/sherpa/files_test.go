package sherpa

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// 模型文件按配置给的名字找：目录里有别的模型文件也不去挑，缺了配置给的那个就报出是哪个文件
// （在交给 sherpa-onnx 之前就报，不会去加载别的文件）。
func TestModelFilesComeFromConfig(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"model.int8.onnx", tokensFile, "keywords.txt",
		"encoder-chunk-16.int8.onnx", "decoder-chunk-16.onnx", "joiner-chunk-16.int8.onnx"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := NewRecognizer(RecognizerConfig{ModelDir: dir, ModelFile: "model.onnx", Threads: 1, Language: "auto"})
	requireMissing(t, err, filepath.Join(dir, "model.onnx"))

	_, _, err = NewKeywordSpotterFactory(KeywordSpotterConfig{ModelDir: dir, EncoderFile: "encoder-chunk-16.int8.onnx",
		DecoderFile: "decoder-chunk-16.int8.onnx", JoinerFile: "joiner-chunk-16.int8.onnx",
		KeywordsFile: filepath.Join(dir, "keywords.txt")})
	requireMissing(t, err, filepath.Join(dir, "decoder-chunk-16.int8.onnx"))
}

// requireMissing 断言 err 是「path 不存在」。
func requireMissing(t *testing.T, err error, path string) {
	t.Helper()
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Errorf("应报出缺的是 %s：%v", path, err)
	}
}
