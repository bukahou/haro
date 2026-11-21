package osdiff

import (
	"reflect"
	"testing"

	"github.com/bukahou/haro/internal/terminal"
)

// pactl -f json list sources 的一段（设备名用文档保留段的假地址）：蓝牙耳机、它的回录源、USB 摄像头的麦克风、板载声卡的回录源。
const pulseSources = `[
 {"index":1,"name":"bluez_input.00:00:5E:00:53:01","properties":{"device.api":"bluez5","device.bus":"bluetooth"}},
 {"index":2,"name":"bluez_output.00:00:5E:00:53:01.monitor","properties":{"device.bus":"bluetooth","device.class":"monitor"}},
 {"index":3,"name":"alsa_input.usb-Example_Camera-02.mono-fallback","properties":{"device.bus":"usb","device.class":"sound"}},
 {"index":4,"name":"alsa_output.pci-0000_00_1f.3.analog-stereo.monitor","properties":{"device.bus":"pci","device.class":"monitor"}},
 {"index":5,"name":"alsa_input.platform-soc_sound.stereo-fallback","properties":{"device.bus":"platform"}},
 {"index":6,"name":"virtual_source","properties":{}}
]`

// 接口类型和回录源看 PipeWire / PulseAudio 报的属性，不看名字。
func TestPulseAudioTraits(t *testing.T) {
	traits, err := pulseAudioTraits([]byte(pulseSources))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AudioTraits{
		"bluez_input.00:00:5E:00:53:01":                      {Bus: terminal.BusBluetooth},
		"bluez_output.00:00:5E:00:53:01.monitor":             {Bus: terminal.BusBluetooth, Monitor: true},
		"alsa_input.usb-Example_Camera-02.mono-fallback":     {Bus: terminal.BusUSB},
		"alsa_output.pci-0000_00_1f.3.analog-stereo.monitor": {Bus: terminal.BusBuiltin, Monitor: true},
		"alsa_input.platform-soc_sound.stereo-fallback":      {Bus: terminal.BusBuiltin},
		"virtual_source": {},
	}
	if !reflect.DeepEqual(traits, want) {
		t.Errorf("得到 %+v\n应为 %+v", traits, want)
	}
	if _, err := pulseAudioTraits([]byte("Connection failure: Connection refused")); err == nil {
		t.Error("不是 JSON 时应报错")
	}
}

// 能列出视频采集格式的节点才是摄像头（元数据节点、打不开的不算），按设备号排。
func TestCameraNodes(t *testing.T) {
	nodes := []v4l2Node{
		{path: "/dev/video11", name: "摄像头"},
		{path: "/dev/video10", name: "摄像头", capture: true},
		{path: "/dev/video2", name: "别的设备"},
		{path: "/dev/video3", name: "第二个摄像头", capture: true},
	}
	want := []terminal.Device{{ID: "/dev/video3", Name: "第二个摄像头"}, {ID: "/dev/video10", Name: "摄像头"}}
	if got := cameraNodes(nodes); !reflect.DeepEqual(got, want) {
		t.Errorf("应只列能拍照的节点、按编号排：%+v", got)
	}
	if got := cameraNodes(nil); len(got) != 0 {
		t.Errorf("没有设备时应为空：%+v", got)
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
