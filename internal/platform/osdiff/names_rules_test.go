package osdiff

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bukahou/haro/internal/terminal"
)

func TestLinuxAudioBus(t *testing.T) {
	cases := map[string]terminal.Bus{
		"bluez_input.00:00:5E:00:53:01":                        terminal.BusBluetooth,
		"bluez_output.00_00_5E_00_53_01.1":                     terminal.BusBluetooth,
		"alsa_input.usb-EMEET_HD_Webcam_C960-02.mono-fallback": terminal.BusUSB,
		"alsa_output.pci-0000_0c_00.4.analog-stereo":           terminal.BusBuiltin,
		"alsa_input.platform-snd_aloop.0.analog-stereo":        terminal.BusBuiltin,
		"something-else": terminal.BusUnknown,
	}
	for id, want := range cases {
		if got := linuxAudioBus(id); got != want {
			t.Errorf("%s：认成 %v，应为 %v", id, got, want)
		}
	}
}

func TestListV4L2CamerasKeepsCaptureNodesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(node, index, name string) {
		os.MkdirAll(filepath.Join(dir, node), 0o755)
		os.WriteFile(filepath.Join(dir, node, "index"), []byte(index+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, node, "name"), []byte(name+"\n"), 0o644)
	}
	if cameras, _ := listV4L2Cameras(dir); len(cameras) != 0 {
		t.Errorf("没有设备时应为空：%+v", cameras)
	}
	write("video11", "1", "摄像头（元数据）")
	write("video10", "0", "摄像头")
	write("video2", "1", "别的元数据")
	write("video3", "0", "第二个摄像头")
	cameras, err := listV4L2Cameras(dir)
	want := []terminal.Device{{ID: "/dev/video3", Name: "第二个摄像头"}, {ID: "/dev/video10", Name: "摄像头"}}
	if err != nil || !reflect.DeepEqual(cameras, want) {
		t.Errorf("应只列 index 为 0 的拍照节点、按编号排：%+v %v", cameras, err)
	}
}

func TestDarwinAudioBus(t *testing.T) {
	cases := map[string]terminal.Bus{
		"00-00-5E-00-53-01:input":                 terminal.BusBluetooth,
		"00-00-5e-00-53-01:output":                terminal.BusBluetooth,
		"AppleUSBAudioEngine:EMEET:HD Webcam:1:1": terminal.BusUSB,
		"BuiltInMicrophoneDevice":                 terminal.BusBuiltin,
		"BuiltInSpeakerDevice":                    terminal.BusBuiltin,
		"com.example.virtual":                     terminal.BusUnknown,
	}
	for id, want := range cases {
		if got := darwinAudioBus(id); got != want {
			t.Errorf("%s：认成 %v，应为 %v", id, got, want)
		}
	}
}

func TestParseAVFoundationCameras(t *testing.T) {
	output := `[AVFoundation indev @ 0x7f9] AVFoundation video devices:
[AVFoundation indev @ 0x7f9] [0] FaceTime HD Camera
[AVFoundation indev @ 0x7f9] [1] EMEET HD Webcam C960
[AVFoundation indev @ 0x7f9] [2] Capture screen 0
[AVFoundation indev @ 0x7f9] AVFoundation audio devices:
[AVFoundation indev @ 0x7f9] [0] MacBook Pro Microphone
[in#0 @ 0x7fa] Error opening input: Input/output error
`
	want := []terminal.Device{{ID: "0", Name: "FaceTime HD Camera"}, {ID: "1", Name: "EMEET HD Webcam C960"}}
	if got := parseAVFoundationCameras(output); !reflect.DeepEqual(got, want) {
		t.Errorf("应只取视频设备、去掉屏幕录制：%+v", got)
	}
	if got := parseAVFoundationCameras("ffmpeg: command not found"); len(got) != 0 {
		t.Errorf("没有设备清单时应为空：%+v", got)
	}
}

func TestCameraInputs(t *testing.T) {
	if got := avfoundationInput("1", "1280x720"); got[1] != "avfoundation" || got[len(got)-1] != "1" {
		t.Errorf("Mac 输入参数不对：%v", got)
	}
}
