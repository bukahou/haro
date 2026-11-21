//go:build linux

package osdiff

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/terminal"
)

// linuxAdapter：音频设备 ID 是 PipeWire / PulseAudio 的节点名，属性从 pactl 的结构化输出取；摄像头走 V4L2。
type linuxAdapter struct {
	devDir string // 设备节点所在的目录（/dev）
	video  v4l2Probe

	mu         sync.Mutex
	audio      map[string]AudioTraits // 按设备 ID 缓存：一个节点的属性不会变
	pactlErr   error                  // 上次查 pactl 失败的原因
	retryAfter time.Time              // 查失败后这之前不再起 pactl（设备清单每隔几秒查一次）
}

var _ Adapter = (*linuxAdapter)(nil) // 少实现一个方法，Linux 版就编译不过

// Current 返回本次编译的系统的实现。
func Current() Adapter { return &linuxAdapter{devDir: "/dev", audio: map[string]AudioTraits{}} }

const (
	pactlTimeout = 3 * time.Second // 列设备只是查询，正常一瞬间就完
	pactlRetry   = time.Minute     // 查不到（没装 pactl、PipeWire 没起来）后隔这么久再试
)

// AudioTraits 认出设备的接口类型和是不是回录源：没见过的设备才去问 pactl（一次列出全部设备）。
func (a *linuxAdapter) AudioTraits(ctx context.Context, id string) (AudioTraits, error) {
	a.mu.Lock()
	traits, known := a.audio[id]
	waiting, lastErr := time.Now().Before(a.retryAfter), a.pactlErr
	a.mu.Unlock()
	switch {
	case known:
		return traits, nil
	case waiting:
		return AudioTraits{}, lastErr
	}
	listed, err := listPulseDevices(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.pactlErr, a.retryAfter = err, time.Now().Add(pactlRetry)
		return AudioTraits{}, err
	}
	maps.Copy(a.audio, listed)
	if _, ok := a.audio[id]; !ok {
		a.audio[id] = AudioTraits{} // pactl 也不知道的设备：当未知，不再为它起进程
	}
	return a.audio[id], nil
}

// listPulseDevices 用 pactl 的 JSON 输出列出全部输入、输出设备的属性。
func listPulseDevices(ctx context.Context) (map[string]AudioTraits, error) {
	ctx, cancel := context.WithTimeout(ctx, pactlTimeout)
	defer cancel()
	traits := map[string]AudioTraits{}
	for _, kind := range []string{"sources", "sinks"} {
		listing, err := exec.CommandContext(ctx, "pactl", "-f", "json", "list", kind).Output()
		if err != nil {
			return nil, fmt.Errorf("用 pactl 列音频设备（%s）: %w", kind, err)
		}
		listed, err := pulseAudioTraits(listing)
		if err != nil {
			return nil, err
		}
		maps.Copy(traits, listed)
	}
	return traits, nil
}

// Cameras 按 V4L2 查每个 /dev/video* 节点，挑出能拍照的（规则见 cameraNodes）。
func (a *linuxAdapter) Cameras(context.Context) ([]terminal.Device, error) {
	paths, err := filepath.Glob(filepath.Join(a.devDir, "video*"))
	if err != nil {
		return nil, err
	}
	nodes := make([]v4l2Node, 0, len(paths))
	for _, path := range paths {
		nodes = append(nodes, a.video.node(path))
	}
	return cameraNodes(nodes), nil
}

// CameraInput 按 V4L2 列出摄像头支持的格式、分辨率和帧率，再选一个明确要（规则见 chooseV4L2Capture）；
// 列不出时照旧由驱动定格式。
func (a *linuxAdapter) CameraInput(_ context.Context, id, captureSize string) CameraCapture {
	return v4l2Capture(id, listV4L2Formats(id), captureSize)
}
