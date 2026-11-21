//go:build integration && linux

package osdiff

import (
	"context"
	"testing"
)

// 接着真摄像头看会向它要什么格式和分辨率（只列格式，不取画面；摄像头正被终端用着也能跑）：
//
//	go test -tags integration -run LiveCameraInput -v ./internal/platform/osdiff/
func TestLiveCameraInput(t *testing.T) {
	ctx := context.Background()
	cameras, err := Current().Cameras(ctx)
	if err != nil || len(cameras) == 0 {
		t.Skipf("没有摄像头：%v", err)
	}
	for _, camera := range cameras {
		capture := Current().CameraInput(ctx, camera.ID, "1280x720")
		t.Logf("%s（%s）：format=%q size=%s input=%v", camera.Name, camera.ID, capture.Format, capture.Size, capture.Input)
		if capture.Format == "" {
			t.Errorf("%s 列不出格式", camera.Name)
		}
	}
}

// 接着真的 PipeWire / PulseAudio 看认出的接口类型和回录源（日志只写类型和数量：设备名里有序列号、蓝牙地址）：
//
//	go test -tags integration -run LiveAudioTraits -v ./internal/platform/osdiff/
func TestLiveAudioTraits(t *testing.T) {
	traits, err := listPulseDevices(context.Background())
	if err != nil {
		t.Skipf("列不出音频设备：%v", err)
	}
	counts := map[AudioTraits]int{}
	for _, trait := range traits {
		counts[trait]++
	}
	for trait, n := range counts {
		t.Logf("接口类型 %s、回录源 %v：%d 个", trait.Bus, trait.Monitor, n)
	}
	if len(traits) == 0 {
		t.Error("一个设备都没有")
	}
}
