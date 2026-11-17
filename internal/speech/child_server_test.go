package speech

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartChildServerReportsEarlyExit(t *testing.T) {
	started := time.Now()
	_, err := StartChildServer(context.Background(), ChildServerSpec{
		Name: "fake", Command: "sh", Args: []string{"-c", "echo 模型文件不存在 >&2; exit 1"},
		BaseURL: "http://127.0.0.1:1", ReadyPath: "/health", ReadyTimeout: 10 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "模型文件不存在") {
		t.Fatalf("应带上子进程的错误输出，得到 %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Error("子进程退出后应立即报错，不应等到超时")
	}
}
