package brain

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/bukahou/haro/internal/docstest"
)

// 会话自己提供的工具都写进了 architecture.md 的工具表，要不要过确认闸和工具说明上的标注一致
// （工具箱里的由 cmd/haro-server 的测试核对，文档里多出来的名字也在那里查）。
func TestSessionToolsAreDocumented(t *testing.T) {
	table := docstest.ToolTable(t, filepath.Join("..", "..", "docs", "architecture.md"))
	session := NewSession(Components{}, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = session.Close() })
	for _, tool := range sessionTools {
		spec, _ := tool.offer(session) // 这一轮提不提供不要紧，只看说明上的标注
		documented, listed := table[tool.name]
		switch {
		case !listed:
			t.Errorf("会话工具 %s 没写进 architecture.md 的工具表", tool.name)
		case documented != spec.RequiresConfirmation:
			t.Errorf("architecture.md 的工具表里 %s 过不过确认闸写的是 %v，代码里是 %v", tool.name, documented, spec.RequiresConfirmation)
		}
	}
}
