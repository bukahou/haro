//go:build integration && linux

package camera

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/platform/osdiff"
	"github.com/bukahou/haro/internal/terminal"
)

// 用这台机器真的摄像头拍一帧（摄像头的灯会亮一下）；画面不存盘。没有摄像头、或被别的程序占用时跳过。
//
//	go test -tags integration -run LiveMediaDevices -v ./internal/platform/camera/
func TestLiveMediaDevicesCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	find := func(ctx context.Context) (string, string, bool) {
		cameras, err := osdiff.Current().Cameras(ctx)
		if err != nil || len(cameras) == 0 {
			return "", "", false
		}
		return cameras[0].ID, cameras[0].Name, true
	}
	if _, _, ok := find(ctx); !ok {
		t.Skip("没有摄像头")
	}
	camera, err := NewMediaDevicesCamera(find, "", terminal.FrameSize{Width: 1280, Height: 720}, 1280,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	frame, err := camera.Capture(ctx, 640)
	if errors.Is(err, terminal.ErrCameraBusy) {
		t.Skip("摄像头被别的程序占用")
	}
	if err != nil {
		t.Fatal(err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG))
	if err != nil || config.Width != frame.Width || frame.Width > 640 {
		t.Errorf("应是不超过 640 宽的 JPEG：%+v %v", config, err)
	}
	t.Logf("拍一帧用了 %s，%dx%d，%d 字节，设备 %s", time.Since(started).Round(time.Millisecond), frame.Width, frame.Height,
		len(frame.JPEG), frame.Device)
}
