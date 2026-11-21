package terminal

import (
	"context"
	"errors"
	"fmt"
)

// 选设备规则：只看 Device 的 Bus / Default，不认操作系统。
// override 是配置指定的设备 ID（HARO_AUDIO_SOURCE / HARO_AUDIO_SINK / HARO_CAMERA_DEVICE）。
// 麦克风、音箱指定了也要在这次列出的设备里：不在（被拔掉、蓝牙断了、ID 写错了）就当没有，看守待机等它回来，
// 而不是拿一个不存在的 ID 去打开、失败、重来。

// CameraNone 是「不用摄像头」的配置值（HARO_CAMERA_DEVICE=none）。
const CameraNone = "none"

var (
	errNoMicrophone = errors.New("没有找到可用的麦克风")
	errNoSpeaker    = errors.New("没有找到可用的音箱")
)

// MissingDeviceError 是配置指定的设备不在这次列出的设备里。Present 是现在有的设备 ID（ID 写错时照着改）。
type MissingDeviceError struct {
	Setting string   // 配置项，例如 HARO_AUDIO_SOURCE
	ID      string   // 配置的设备 ID
	Present []string // 现在列出的设备 ID
}

func (e *MissingDeviceError) Error() string {
	return fmt.Sprintf("配置指定的设备现在不在：%s=%q（现在有：%q）", e.Setting, e.ID, e.Present)
}

// findConfigured 按 ID 在列出的设备里找配置指定的那个。
func findConfigured(devices []Device, setting, id string) (Device, error) {
	present := make([]string, 0, len(devices))
	for _, device := range devices {
		if device.ID == id {
			return device, nil
		}
		present = append(present, device.ID)
	}
	return Device{}, &MissingDeviceError{Setting: setting, ID: id, Present: present}
}

// ChooseMicrophone 跟系统默认，但有别的麦克风时不用蓝牙麦克风（蓝牙麦克风会把耳机切到通话模式）。
// 配置指定了就用指定的那个（不在时返回 *MissingDeviceError）。
func ChooseMicrophone(devices []Device, override string) (Choice, error) {
	if override != "" {
		device, err := findConfigured(devices, "HARO_AUDIO_SOURCE", override)
		if err != nil {
			return Choice{}, err
		}
		return Choice{ID: device.ID, Name: device.Name, Reason: "配置指定（HARO_AUDIO_SOURCE）"}, nil
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
// 配置指定了就用指定的那个（不在时返回 *MissingDeviceError）。
func ChooseSpeaker(devices []Device, override string) (Choice, error) {
	if override != "" {
		device, err := findConfigured(devices, "HARO_AUDIO_SINK", override)
		if err != nil {
			return Choice{}, err
		}
		return Choice{ID: device.ID, Name: device.Name, Reason: "配置指定（HARO_AUDIO_SINK）"}, nil
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
