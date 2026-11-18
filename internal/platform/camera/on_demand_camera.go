// Package camera 是平台层的取帧实现：用 ffmpeg 从摄像头取 JPEG。
// 各操作系统只是 ffmpeg 的输入参数不同（Linux：-f v4l2；macOS：-f avfoundation），由 Locate 给出，这里不认系统。
package camera

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// errNoCamera：现在找不到摄像头。
var errNoCamera = errors.New("现在没有摄像头")

// OnDemandCamera 每次调用才用 ffmpeg 抓一帧（约 0.8 秒）。
// 用于 HARO_CAMERA_MODE=on-demand：摄像头平时不开。抓失败后，下一次先重新找摄像头。
type OnDemandCamera struct {
	locate      Locate
	outputWidth int // 送给模型的最大宽度（画面更小就原样送），高度按比例
	health      *health
	frameSize   *frameSizeLog

	mu    sync.Mutex
	input []string // 当前摄像头的输入参数；nil = 下次先重新找
}

// NewOnDemandCamera 创建按需抓帧器；name 是启动时选中的摄像头。
func NewOnDemandCamera(locate Locate, name string, outputWidth int, logger *slog.Logger) *OnDemandCamera {
	return &OnDemandCamera{locate: locate, outputWidth: outputWidth, health: newHealth(name, logger),
		frameSize: newFrameSizeLog(logger)}
}

// LatestFrame 现抓一帧（JPEG）。
func (c *OnDemandCamera) LatestFrame(ctx context.Context) ([]byte, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.health.currentName()
	if c.input == nil {
		input, found, ok := c.locate(ctx)
		if !ok {
			c.health.markDown("没有找到摄像头", nil)
			return nil, time.Time{}, errNoCamera
		}
		c.input, name = input, found
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{"-hide_banner", "-loglevel", "error"}, c.input...)
	args = append(args, "-frames:v", "1", "-vf", scaleFilter(c.outputWidth),
		"-f", "image2", "-c:v", "mjpeg", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		c.input = nil // 下次先重新找（可能被拔掉了，或换了设备号）
		err = fmt.Errorf("ffmpeg 抓帧: %w: %s", err, stderr.String())
		c.health.markDown("抓帧失败", err)
		return nil, time.Time{}, err
	}
	c.health.markUp(name)
	c.frameSize.observe(stdout.Bytes())
	return stdout.Bytes(), time.Now(), nil
}

// CurrentName 是现在能用的摄像头名字；不可用时为空（看守据此告诉服务端）。
func (c *OnDemandCamera) CurrentName() string { return c.health.currentName() }
