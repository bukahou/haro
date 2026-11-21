package osdiff

import (
	"encoding/json"
	"fmt"

	"github.com/bukahou/haro/internal/terminal"
)

// 这个文件不带构建标签：Linux 上认音频设备的规则在任何机器上都能测。

// pulseDevice 是 `pactl -f json list sources` / `list sinks` 里的一个设备（只取用到的字段）。
// name 就是 miniaudio 报的设备 ID（PipeWire / PulseAudio 的节点名）。
type pulseDevice struct {
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
}

// pulseAudioTraits 按 PipeWire / PulseAudio 报的设备属性认接口类型和回录源（看结构化的属性，不按名字猜）：
//
//	device.bus   ：bluetooth / usb / pci、platform（板载声卡、HDMI）
//	device.class ：monitor = 回录源
func pulseAudioTraits(listing []byte) (map[string]AudioTraits, error) {
	var devices []pulseDevice
	if err := json.Unmarshal(listing, &devices); err != nil {
		return nil, fmt.Errorf("pactl 的设备清单看不懂: %w", err)
	}
	traits := make(map[string]AudioTraits, len(devices))
	for _, device := range devices {
		traits[device.Name] = AudioTraits{Bus: pulseBus(device.Properties["device.bus"]),
			Monitor: device.Properties["device.class"] == "monitor"}
	}
	return traits, nil
}

// pulseBus 把 device.bus 的取值换成接口类型；没报或认不出的当未知。
func pulseBus(bus string) terminal.Bus {
	switch bus {
	case "bluetooth":
		return terminal.BusBluetooth
	case "usb":
		return terminal.BusUSB
	case "pci", "platform":
		return terminal.BusBuiltin
	default:
		return terminal.BusUnknown
	}
}
