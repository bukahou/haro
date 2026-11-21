// Package audio 是平台层的音频实现（v4 D10）：用 miniaudio（malgo，cgo）录音、放音、列设备，
// 一套代码跑 Linux（经 PipeWire 的 PulseAudio 兼容层）/ macOS（CoreAudio）/ 树莓派。
//
// 实现 assistant 包的 Microphone / AudioRecorder / AudioSink 接口；只由平台层门面（internal/platform）使用，选哪个设备的规则在服务层（internal/terminal）。
// 设备用「键」（Key）指认：miniaudio 设备 ID 里的字符串——PulseAudio 下就是节点名（与 PipeWire 的 node.name 一致），
// CoreAudio 下是设备 UID。
//
// ⚠️ miniaudio 只在「正在用的设备停掉」（例如被拔掉）时通知；新设备插上来不会通知，要靠定期 Devices() 查（v4 D3）。
package audio

// #include <stdlib.h>
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Backend 持有一个 miniaudio 上下文（整个终端进程一个），所有设备都从它打开。
type Backend struct {
	context *malgo.AllocatedContext
	logger  *slog.Logger
	mu      sync.Mutex // 打开 / 关闭设备时串行（miniaudio 的上下文不保证并发安全）
}

// Device 是列出来的一个设备。
type Device struct {
	Key     string // 设备 ID 里的字符串（用来指认、比较）
	Name    string // 给人看的名字
	Default bool   // 是不是系统默认
	id      malgo.DeviceID
}

// NewMiniaudioBackend 初始化 miniaudio（按平台默认顺序选后端：Linux 先 PulseAudio，macOS 用 CoreAudio）。
func NewMiniaudioBackend(logger *slog.Logger) (*Backend, error) {
	context, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		logger.Debug("miniaudio", "message", strings.TrimSpace(message))
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 miniaudio: %w", err)
	}
	return &Backend{context: context, logger: logger}, nil
}

// Close 释放上下文（所有设备都已关闭之后调用）。
func (b *Backend) Close() {
	_ = b.context.Uninit()
	b.context.Free()
}

// Microphones 列出麦克风（不含「Monitor of …」这类录扬声器输出的虚拟源）。
func (b *Backend) Microphones() ([]Device, error) { return b.devices(malgo.Capture) }

// Speakers 列出音箱。
func (b *Backend) Speakers() ([]Device, error) { return b.devices(malgo.Playback) }

func (b *Backend) devices(kind malgo.DeviceType) ([]Device, error) {
	b.mu.Lock()
	infos, err := b.context.Devices(kind)
	b.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("列出音频设备: %w", err)
	}
	devices := make([]Device, 0, len(infos))
	for _, info := range infos {
		// 回录源（录的是放出来的声音）由平台层按系统报的属性去掉（osdiff.AudioTraits），这里照实列出
		devices = append(devices, Device{Key: deviceKey(info.ID), Name: info.Name(), Default: info.IsDefault != 0, id: info.ID})
	}
	return devices, nil
}

// deviceKey 取设备 ID 里的字符串（到第一个 0 字节为止）；不是可打印文字时用十六进制。
func deviceKey(id malgo.DeviceID) string {
	raw := id[:]
	if end := bytes.IndexByte(raw, 0); end >= 0 {
		raw = raw[:end]
	}
	for _, c := range raw {
		if c < 0x20 || c > 0x7e {
			return id.String()
		}
	}
	return string(raw)
}

// errDeviceGone：要打开的设备已经不在了（被拔掉、蓝牙断开）。
var errDeviceGone = errors.New("音频设备不在了")

// findDevice 按键找设备；key 为空表示用系统默认（返回 nil）。
func (b *Backend) findDevice(kind malgo.DeviceType, key string) (*malgo.DeviceID, error) {
	if key == "" {
		return nil, nil
	}
	devices, err := b.devices(kind)
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.Key == key {
			id := device.id
			return &id, nil
		}
	}
	return nil, fmt.Errorf("%w：%s", errDeviceGone, key)
}

// openDevice 按配置打开并启动一个设备。id 为 nil = 系统默认（跟随切换）。
func (b *Backend) openDevice(config malgo.DeviceConfig, callbacks malgo.DeviceCallbacks) (*malgo.Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	device, err := malgo.InitDevice(b.context.Context, config, callbacks)
	if err != nil {
		return nil, err
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		return nil, err
	}
	return device, nil
}

// closeDevice 停止并释放设备。⚠️ 不能在 miniaudio 的回调里调用（会死锁）。
func (b *Backend) closeDevice(device *malgo.Device) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = device.Stop()
	device.Uninit()
}

// cDeviceID 把设备 ID 复制到 C 内存交给 malgo（Go 指针不能放进传给 C 的结构体）；nil = 系统默认。
// 返回的 release 在设备关闭后调用。
func cDeviceID(id *malgo.DeviceID) (unsafe.Pointer, func()) {
	if id == nil {
		return nil, func() {}
	}
	pointer := id.Pointer() // C.CBytes
	return pointer, func() { C.free(pointer) }
}
