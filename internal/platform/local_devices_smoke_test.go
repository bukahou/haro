//go:build audiohw

// 真机冒烟测试：go test -tags audiohw -v ./internal/platform
// 只列设备（不录音、不放音）：看平台层在这台机器上认出的接口类型、选设备规则选中谁。
package platform

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/bukahou/haro/internal/terminal"
)

func TestLocalDevicesSmoke(t *testing.T) {
	local, err := OpenLocalDevices(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	microphones, err := local.Microphones()
	if err != nil {
		t.Fatal(err)
	}
	speakers, err := local.Speakers()
	if err != nil {
		t.Fatal(err)
	}
	cameras, err := local.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []struct {
		kind    string
		devices []terminal.Device
	}{{"麦克风", microphones}, {"音箱", speakers}, {"摄像头", cameras}} {
		for _, device := range group.devices {
			t.Logf("%s：%s ［%s］默认=%v id=%s", group.kind, device.Name, device.Bus, device.Default, device.ID)
		}
	}
	microphone, err := terminal.ChooseMicrophone(microphones, "")
	t.Logf("→ 选中麦克风：%s（%s）%v", microphone.Name, microphone.Reason, err)
	speaker, err := terminal.ChooseSpeaker(speakers, "")
	t.Logf("→ 选中音箱：%s（%s）%v", speaker.Name, speaker.Reason, err)
	camera := terminal.ChooseCamera(cameras, "")
	t.Logf("→ 选中摄像头：%s（%s）", camera.Name, camera.Reason)
}
