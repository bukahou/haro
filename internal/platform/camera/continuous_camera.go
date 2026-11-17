package camera

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// ContinuousCamera 让 ffmpeg 常驻，以低帧率持续输出 JPEG，只保留最新一帧。
// 取帧没有等待，拿到的是「说完那一刻」的画面。代价：haro 运行期间摄像头一直开着。
// ffmpeg 退出（例如摄像头被拔掉）后，先重新找摄像头再重启，间隔逐步拉长到 10 秒；只在状态变化时写日志。
type ContinuousCamera struct {
	locate      Locate
	outputWidth int
	fps         int           // 输出帧率；只是为了随时有新画面，2 就够
	maxAge      time.Duration // 超过这个时长没有新帧，视为摄像头出问题
	retryMin    time.Duration // 重试间隔：从这么久开始翻倍
	retryMax    time.Duration
	health      *health

	mu      sync.Mutex
	latest  []byte
	takenAt time.Time
}

// NewContinuousCamera 创建常驻取帧器（name 是启动时选中的摄像头）；需要另外调用 Run 启动。
func NewContinuousCamera(locate Locate, name string, outputWidth, fps int, maxAge time.Duration,
	logger *slog.Logger) *ContinuousCamera {
	return &ContinuousCamera{locate: locate, outputWidth: outputWidth, fps: fps, maxAge: maxAge,
		retryMin: time.Second, retryMax: 10 * time.Second, health: newHealth(name, logger)}
}

// Run 找摄像头、运行 ffmpeg；退出了就重新找、重启，直到 ctx 取消。
func (c *ContinuousCamera) Run(ctx context.Context) {
	backoff := c.retryMin
	for {
		started := time.Now()
		if input, name, ok := c.locate(ctx); !ok {
			c.health.markDown("没有找到摄像头", nil)
		} else {
			err := c.runFFmpeg(ctx, input, name)
			if ctx.Err() != nil {
				return
			}
			c.health.markDown("摄像头进程退出", err)
		}
		if time.Since(started) > time.Minute {
			backoff = c.retryMin // 跑了一阵才退出，说明不是持续性故障
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.retryMax)
	}
}

// CurrentName 是现在能用的摄像头名字；不可用时为空（看守据此告诉服务端）。
func (c *ContinuousCamera) CurrentName() string { return c.health.currentName() }

func (c *ContinuousCamera) runFFmpeg(ctx context.Context, input []string, name string) error {
	filter := "fps=" + strconv.Itoa(c.fps) + ",scale=" + strconv.Itoa(c.outputWidth) + ":-2"
	args := append([]string{"-hide_banner", "-loglevel", "error"}, input...)
	args = append(args, "-vf", filter, "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "5", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 ffmpeg: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 8*1024*1024)
	scanner.Split(splitJPEGFrames)
	for scanner.Scan() {
		frame := bytes.Clone(scanner.Bytes())
		c.mu.Lock()
		c.latest, c.takenAt = frame, time.Now()
		c.mu.Unlock()
		c.health.markUp(name)
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return fmt.Errorf("ffmpeg 退出: %v: %s", waitErr, stderr.String())
}

// LatestFrame 返回最新一帧（JPEG）及拍摄时刻。
func (c *ContinuousCamera) LatestFrame(ctx context.Context) ([]byte, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latest == nil {
		return nil, time.Time{}, errors.New("摄像头还没有画面")
	}
	if age := time.Since(c.takenAt); age > c.maxAge {
		return nil, time.Time{}, fmt.Errorf("最新画面已经是 %s 前的了", age.Round(time.Millisecond))
	}
	return c.latest, c.takenAt, nil
}

// splitJPEGFrames 是 bufio.SplitFunc：从 MJPEG 字节流里按 SOI(FFD8)…EOI(FFD9) 切出完整 JPEG。
// JPEG 的熵编码数据会把 0xFF 转义成 FF00，所以 FFD9 只会出现在帧尾（ffmpeg 输出不含 EXIF 缩略图）。
func splitJPEGFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := bytes.Index(data, []byte{0xFF, 0xD8})
	if start < 0 {
		if atEOF {
			return len(data), nil, nil
		}
		return max(len(data)-1, 0), nil, nil // 丢掉垃圾数据，保留最后一个字节以防 FF 被截断
	}
	end := bytes.Index(data[start+2:], []byte{0xFF, 0xD9})
	if end < 0 {
		if atEOF {
			return len(data), nil, nil // 流结束时的半帧直接丢弃
		}
		return start, nil, nil // 等更多数据
	}
	frameEnd := start + 2 + end + 2
	return frameEnd, data[start:frameEnd], nil
}
