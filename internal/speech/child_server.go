package speech

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ChildServerSpec 描述一个由 haro 托管的本地 HTTP 服务（Piper 合成、haro-asr 识别）。
type ChildServerSpec struct {
	Name         string        // 用于报错，例如 "piper"
	Command      string        // 可执行文件
	Args         []string      // 参数（应让服务只监听 127.0.0.1）
	BaseURL      string        // 服务根地址，例如 http://127.0.0.1:59125
	ReadyPath    string        // 就绪检查路径，GET 返回 200 即就绪
	ReadyTimeout time.Duration // 等待就绪的上限（含模型加载）
	LogOutput    io.Writer     // 非 nil 时把子进程的错误输出（日志）同时转发到这里；nil 只在启动失败时用
}

// ChildServer 是一个随 haro 启动、随 haro 退出的子进程服务。
type ChildServer struct {
	BaseURL string
	name    string
	cmd     *exec.Cmd
	exited  chan struct{}
	stderr  *tailBuffer
}

// StartChildServer 启动服务并等到它能响应请求为止；启动失败时带上服务最后的错误输出。
func StartChildServer(ctx context.Context, spec ChildServerSpec) (*ChildServer, error) {
	server := &ChildServer{
		BaseURL: spec.BaseURL,
		name:    spec.Name,
		exited:  make(chan struct{}),
		stderr:  &tailBuffer{limit: 4096},
	}
	server.cmd = exec.Command(spec.Command, spec.Args...)
	server.cmd.Stderr = server.stderr
	if spec.LogOutput != nil {
		server.cmd.Stderr = io.MultiWriter(server.stderr, spec.LogOutput)
	}
	server.cmd.SysProcAttr = childProcAttr()
	if err := server.cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 服务: %w", spec.Name, err)
	}
	go func() {
		_ = server.cmd.Wait()
		close(server.exited)
	}()
	if err := server.waitReady(ctx, spec.BaseURL+spec.ReadyPath, spec.ReadyTimeout); err != nil {
		server.Stop()
		return nil, err
	}
	return server, nil
}

func (s *ChildServer) waitReady(ctx context.Context, readyURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-s.exited:
			return fmt.Errorf("%s 服务启动失败: %s", s.name, s.stderr.String())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		resp, err := client.Get(readyURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return fmt.Errorf("%s 服务 %s 内没有就绪: %s", s.name, timeout, s.stderr.String())
}

// Stop 先礼貌地结束（SIGTERM），3 秒内没退出就强制结束。
func (s *ChildServer) Stop() {
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
	}
}

// tailBuffer 只保留最后 limit 字节的输出，用于启动失败时报错。
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
