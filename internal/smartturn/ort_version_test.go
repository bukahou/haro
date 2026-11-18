//go:build linux && amd64

package smartturn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// onnxruntime_go 必须和 sherpa-onnx 自带的 onnxruntime 动态库对得上（detector.go 的说明）：
// 动态库只认它那个版本及以前的 API，onnxruntime_go 单独升级后要的 API 更新，编译和单元测试都照常通过，
// 到运行时 Smart Turn 才初始化失败（2026-11-17 Dependabot 提议 v1.32.1（API 28）→ v1.38.0（API 31），而自带的是 1.28.2）。
// 这里用 sherpa-onnx 模块里的那份动态库真的初始化一次：对不上就在 CI 里失败。
func TestBundledOnnxRuntimeSupportsBinding(t *testing.T) {
	moduleDir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/k2-fsa/sherpa-onnx-go-linux").Output()
	if err != nil {
		t.Fatalf("找不到 sherpa-onnx-go-linux 模块：%v", err)
	}
	library := filepath.Join(strings.TrimSpace(string(moduleDir)), "lib", "x86_64-unknown-linux-gnu", "libonnxruntime.so")
	if _, err := os.Stat(library); err != nil {
		t.Fatalf("sherpa-onnx 模块里没有 onnxruntime 动态库：%v", err)
	}
	ort.SetSharedLibraryPath(library)
	if err := ort.InitializeEnvironment(); err != nil {
		t.Fatalf("onnxruntime_go 用不了 sherpa-onnx 自带的 onnxruntime（两者的版本要一起对齐，见 detector.go）：%v", err)
	}
	if err := ort.DestroyEnvironment(); err != nil {
		t.Errorf("DestroyEnvironment: %v", err)
	}
}
