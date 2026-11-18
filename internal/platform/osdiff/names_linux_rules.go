package osdiff

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bukahou/haro/internal/terminal"
)

// 这个文件不带构建标签（文件名不以 _linux.go 结尾）：Linux 的解析规则在任何机器上都能测。

// linuxAudioBus 按 PulseAudio / PipeWire 的节点名认接口类型：
//
//	bluez_input.XX:XX…  / bluez_output.…  → 蓝牙
//	alsa_input.usb-…    / alsa_output.usb-… → USB
//	alsa_input.pci-…    / …platform-…      → 内置（声卡、HDMI）
func linuxAudioBus(id string) terminal.Bus {
	switch {
	case strings.HasPrefix(id, "bluez_"):
		return terminal.BusBluetooth
	case strings.Contains(id, ".usb-"):
		return terminal.BusUSB
	case strings.Contains(id, ".pci-"), strings.Contains(id, ".platform-"):
		return terminal.BusBuiltin
	default:
		return terminal.BusUnknown
	}
}

// listV4L2Cameras 从 /sys/class/video4linux 列摄像头。一个摄像头常有好几个节点
// （EMEET C960 有 video0 / video1），只有 index 为 0 的是拍照用的，其余是元数据。
func listV4L2Cameras(sysVideo string) ([]terminal.Device, error) {
	entries, err := filepath.Glob(filepath.Join(sysVideo, "video*"))
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return videoNumber(entries[i]) < videoNumber(entries[j]) })
	var cameras []terminal.Device
	for _, entry := range entries {
		if index, err := os.ReadFile(filepath.Join(entry, "index")); err != nil || strings.TrimSpace(string(index)) != "0" {
			continue
		}
		name, _ := os.ReadFile(filepath.Join(entry, "name"))
		cameras = append(cameras, terminal.Device{ID: "/dev/" + filepath.Base(entry),
			Name: strings.TrimSpace(string(name)), Bus: terminal.BusUnknown})
	}
	return cameras, nil
}

func videoNumber(path string) int {
	number, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "video"))
	if err != nil {
		return 1 << 30
	}
	return number
}
