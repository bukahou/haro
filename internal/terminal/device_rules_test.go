package terminal

import (
	"errors"
	"slices"
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
		name    string
		devices []Device
		wantID  string
	}{
		{"默认是 USB：用它", []Device{bt, defaultUSB}, usb.ID},
		{"默认是蓝牙、还有别的：不用蓝牙", []Device{defaultBT, usb}, usb.ID},
		{"只剩蓝牙：用它", []Device{defaultBT}, bt.ID},
		{"没有默认：用第一个非蓝牙", []Device{bt, usb}, usb.ID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if choice, err := ChooseMicrophone(c.devices, ""); err != nil || choice.ID != c.wantID {
				t.Errorf("选了 %+v（%v），应为 %s", choice, err, c.wantID)
			}
		})
	}
	if _, err := ChooseMicrophone(nil, ""); !errors.Is(err, errNoMicrophone) {
		t.Errorf("没有麦克风应报没有：%v", err)
	}
}

func TestChooseSpeakerFollowsDefault(t *testing.T) {
	choice, err := ChooseSpeaker([]Device{{ID: "hdmi", Name: "HDMI"}, {ID: "bt", Name: "EBS-313", Default: true, Bus: BusBluetooth}}, "")
	if err != nil || choice.ID != "" || choice.Name != "EBS-313" {
		t.Errorf("音箱应跟系统默认（不指定 ID）、名字取默认的：%+v %v", choice, err)
	}
	if _, err := ChooseSpeaker(nil, ""); !errors.Is(err, errNoSpeaker) {
		t.Errorf("没有音箱应报没有：%v", err)
	}
}

// 配置指定的设备：在就用它（不看默认、不管是不是蓝牙）；不在就报不在，并列出现有的 ID。
func TestChooseConfiguredDevice(t *testing.T) {
	usb := Device{ID: "usb-mic", Name: "USB 麦克风", Default: true, Bus: BusUSB}
	headset := Device{ID: "bt-headset", Name: "蓝牙耳机", Bus: BusBluetooth}
	if choice, err := ChooseMicrophone([]Device{usb, headset}, headset.ID); err != nil || choice.ID != headset.ID || choice.Name != headset.Name {
		t.Errorf("指定的麦克风在：应该用它：%+v %v", choice, err)
	}
	if choice, err := ChooseSpeaker([]Device{usb, headset}, headset.ID); err != nil || choice.ID != headset.ID || choice.Name != headset.Name {
		t.Errorf("指定的音箱在：应该按 ID 用它，不跟默认：%+v %v", choice, err)
	}
	for name, choose := range map[string]func([]Device, string) (Choice, error){"麦克风": ChooseMicrophone, "音箱": ChooseSpeaker} {
		_, err := choose([]Device{usb}, headset.ID)
		var missing *MissingDeviceError
		if !errors.As(err, &missing) || missing.ID != headset.ID || !slices.Equal(missing.Present, []string{usb.ID}) {
			t.Errorf("指定的%s不在：应报不在并列出现有的 ID：%v", name, err)
		}
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
