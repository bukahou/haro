package terminal

import (
	"strings"
	"testing"
)

func TestChooseMicrophone(t *testing.T) {
	usb := Device{ID: "usb-cam", Name: "USB 摄像头", Bus: BusUSB}
	bt := Device{ID: "bt-speaker", Name: "蓝牙音箱", Bus: BusBluetooth}
	defaultBT := bt
	defaultBT.Default = true
	defaultUSB := usb
	defaultUSB.Default = true

	cases := []struct {
		name       string
		devices    []Device
		wantID     string
		wantReason string
	}{
		{"默认是 USB：用它", []Device{bt, defaultUSB}, usb.ID, "系统默认"},
		{"默认是蓝牙、还有别的：不用蓝牙", []Device{defaultBT, usb}, usb.ID, "蓝牙"},
		{"只剩蓝牙：用它", []Device{defaultBT}, bt.ID, "只有蓝牙"},
		{"没有默认：用第一个非蓝牙", []Device{bt, usb}, usb.ID, "没有默认"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			choice, err := ChooseMicrophone(c.devices, "")
			if err != nil || choice.ID != c.wantID || !strings.Contains(choice.Reason, c.wantReason) {
				t.Errorf("选了 %+v（%v），应为 %s / 原因含 %q", choice, err, c.wantID, c.wantReason)
			}
		})
	}
	if _, err := ChooseMicrophone(nil, ""); err == nil {
		t.Error("没有麦克风应报错")
	}
	if choice, _ := ChooseMicrophone([]Device{defaultBT}, "手动"); choice.ID != "手动" {
		t.Errorf("配置指定的优先：%+v", choice)
	}
}

func TestChooseSpeakerFollowsDefault(t *testing.T) {
	choice, err := ChooseSpeaker([]Device{{ID: "hdmi", Name: "HDMI"}, {ID: "bt", Name: "EBS-313", Default: true, Bus: BusBluetooth}}, "")
	if err != nil || choice.ID != "" || choice.Name != "EBS-313" {
		t.Errorf("音箱应跟系统默认（不指定 ID）、名字取默认的：%+v %v", choice, err)
	}
	if _, err := ChooseSpeaker(nil, ""); err == nil {
		t.Error("没有音箱应报错")
	}
	if choice, _ := ChooseSpeaker(nil, "手动"); choice.ID != "手动" {
		t.Errorf("配置指定的优先：%+v", choice)
	}
}

func TestChooseCamera(t *testing.T) {
	cameras := []Device{{ID: "cam0", Name: "EMEET"}, {ID: "cam1", Name: "别的"}}
	if choice := ChooseCamera(nil, ""); choice.ID != "" {
		t.Errorf("没有设备时不用摄像头：%+v", choice)
	}
	if choice := ChooseCamera(cameras, ""); choice.ID != "cam0" || choice.Name != "EMEET" {
		t.Errorf("应选第一个：%+v", choice)
	}
	if choice := ChooseCamera(cameras, CameraNone); choice.ID != "" {
		t.Errorf("none 应关闭摄像头：%+v", choice)
	}
	if choice := ChooseCamera(nil, "/dev/video5"); choice.ID != "/dev/video5" {
		t.Errorf("配置指定的直接用：%+v", choice)
	}
}
