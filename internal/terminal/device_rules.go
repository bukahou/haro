package terminal

import (
	"context"
	"errors"
)

// 选设备规则（v3 H-1 / H-2，v4 第 5 步从 hardware 包搬来）：只看 Device 的 Bus / Default，不认操作系统。
// override 是配置指定的设备 ID（HARO_AUDIO_SOURCE / HARO_AUDIO_SINK / HARO_CAMERA_DEVICE），非空时直接用（P-2）。

// CameraNone 是「不用摄像头」的配置值（HARO_CAMERA_DEVICE=none）。
const CameraNone = "none"

var (
	errNoMicrophone = errors.New("没有找到可用的麦克风")
	errNoSpeaker    = errors.New("没有找到可用的音箱")
)

// ChooseMicrophone 跟系统默认，但有别的麦克风时不用蓝牙麦克风（蓝牙麦克风会把耳机切到通话模式）。
func ChooseMicrophone(devices []Device, override string) (Choice, error) {
	if override != "" {
		return Choice{ID: override, Name: override, Reason: "配置指定（HARO_AUDIO_SOURCE）"}, nil
	}
	var candidates, bluetooth []Device
	defaultIsBluetooth := false
	for _, device := range devices {
		if device.Bus == BusBluetooth {
			bluetooth = append(bluetooth, device)
			defaultIsBluetooth = defaultIsBluetooth || device.Default
			continue
		}
		candidates = append(candidates, device)
	}
	if len(candidates) == 0 {
		if len(bluetooth) == 0 {
			return Choice{}, errNoMicrophone
		}
		chosen := bluetooth[0]
		for _, device := range bluetooth {
			if device.Default {
				chosen = device
			}
		}
		return Choice{ID: chosen.ID, Name: chosen.Name,
			Reason: "只有蓝牙麦克风：用它（蓝牙设备会切到通话模式，放出来的声音也降成单声道 16 kHz）"}, nil
	}
	for _, device := range candidates {
		if device.Default {
			return Choice{ID: device.ID, Name: device.Name, Reason: "系统默认"}, nil
		}
	}
	reason := "系统没有默认麦克风，选列表里第一个"
	if defaultIsBluetooth {
		reason = "系统默认的是蓝牙麦克风（会把耳机切到通话模式），改用列表里第一个非蓝牙的"
	}
	return Choice{ID: candidates[0].ID, Name: candidates[0].Name, Reason: reason}, nil
}

// ChooseSpeaker 跟系统默认：ID 留空（每轮打开时用当时的默认设备，蓝牙重连后自动接回），只查出名字写日志。
func ChooseSpeaker(devices []Device, override string) (Choice, error) {
	if override != "" {
		return Choice{ID: override, Name: override, Reason: "配置指定（HARO_AUDIO_SINK）"}, nil
	}
	if len(devices) == 0 {
		return Choice{}, errNoSpeaker
	}
	for _, device := range devices {
		if device.Default {
			return Choice{Name: device.Name, Reason: "系统默认（跟随切换）"}, nil
		}
	}
	return Choice{Name: "系统默认", Reason: "系统默认（跟随切换）"}, nil
}

// ChooseCamera 选摄像头：none ⇒ 不用；指定了 ⇒ 用它；否则用列表里第一个。ID 为空表示不用摄像头。
func ChooseCamera(devices []Device, override string) Choice {
	switch override {
	case CameraNone:
		return Choice{Reason: "配置关闭（HARO_CAMERA_DEVICE=none）"}
	case "":
	default:
		return Choice{ID: override, Name: override, Reason: "配置指定（HARO_CAMERA_DEVICE）"}
	}
	if len(devices) == 0 {
		return Choice{Reason: "没有检测到摄像头"}
	}
	return Choice{ID: devices[0].ID, Name: devices[0].Name, Reason: "自动检测"}
}

// PickCameraFrom 把「列摄像头 + 选择规则」组成 CameraPicker（取帧失败后由平台层调用，重新找摄像头）。
func PickCameraFrom(catalog DeviceCatalog, override string) CameraPicker {
	return func(ctx context.Context) (Choice, bool) {
		cameras, err := catalog.Cameras(ctx)
		if err != nil {
			return Choice{}, false
		}
		choice := ChooseCamera(cameras, override)
		return choice, choice.ID != ""
	}
}
