// Package tones 是终端在大脑连不上时自己播的提示音（v4 第 1 步 1-B）：大脑断开时没有语音合成可用，
// 所以提示音是提前做好的。
//
// 编进程序里的是不含人声的提示音（gen_tones.go 生成：断开 = 由高到低两声，回来 = 由低到高三声，
// 去休眠 = 轻轻的由高到低两声，提示 = 明亮的「叮咚」）。
// 想用自己的声音录的提示音（例如「我和大脑断开了」「我回来了」「我去睡觉了」）：把 offline.wav、online.wav、
// sleep.wav、notice.wav 放进一个目录，设 HARO_TONES_DIR 指向它（Load；缺哪个就用内置的那个）。
package tones

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"haro/internal/realtimeproto"
)

//go:embed offline.wav
var offlineWAV []byte

//go:embed online.wav
var onlineWAV []byte

//go:embed sleep.wav
var sleepWAV []byte

//go:embed notice.wav
var noticeWAV []byte

// Tones 是终端的全部提示音。
type Tones struct {
	Offline realtimeproto.PCMAudio // 大脑断开了
	Online  realtimeproto.PCMAudio // 大脑回来了
	Sleep   realtimeproto.PCMAudio // 对话结束、去休眠了
	Notice  realtimeproto.PCMAudio // 大脑的提示（例如后台调查查完了）：叫醒问就行
}

// Offline 返回「断开了」的提示音。
func Offline() (realtimeproto.PCMAudio, error) { return ParseWAV(offlineWAV) }

// Online 返回「回来了」的提示音。
func Online() (realtimeproto.PCMAudio, error) { return ParseWAV(onlineWAV) }

// Load 读提示音：dir 为空用编进程序里的；否则读 dir 里的 offline.wav、online.wav、sleep.wav、notice.wav（缺哪个就用内置的那个）。
func Load(dir string) (Tones, error) {
	var tones Tones
	for _, tone := range []struct {
		file    string
		builtin []byte
		target  *realtimeproto.PCMAudio
	}{{"offline.wav", offlineWAV, &tones.Offline}, {"online.wav", onlineWAV, &tones.Online}, {"sleep.wav", sleepWAV, &tones.Sleep},
		{"notice.wav", noticeWAV, &tones.Notice}} {
		data := tone.builtin
		if dir != "" {
			custom, err := os.ReadFile(filepath.Join(dir, tone.file))
			switch {
			case err == nil:
				data = custom
			case !errors.Is(err, os.ErrNotExist):
				return tones, err
			}
		}
		audio, err := ParseWAV(data)
		if err != nil {
			return tones, fmt.Errorf("提示音 %s: %w", tone.file, err)
		}
		*tone.target = audio
	}
	return tones, nil
}

// ParseWAV 读 16 bit PCM 的 WAV（跳过 data 以外的块）。
func ParseWAV(wav []byte) (realtimeproto.PCMAudio, error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return realtimeproto.PCMAudio{}, errors.New("不是 WAV 文件")
	}
	var format realtimeproto.AudioFormat
	for offset := 12; offset+8 <= len(wav); {
		id := string(wav[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		body := offset + 8
		if body+size > len(wav) {
			size = len(wav) - body // 有的合成服务流式写 WAV，data 长度填的是占位值
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return realtimeproto.PCMAudio{}, errors.New("WAV 的 fmt 块太短")
			}
			if audioFormat, bits := binary.LittleEndian.Uint16(wav[body:]), binary.LittleEndian.Uint16(wav[body+14:]); audioFormat != 1 || bits != 16 {
				return realtimeproto.PCMAudio{}, fmt.Errorf("只支持 16 bit PCM 的 WAV（格式 %d，%d bit）", audioFormat, bits)
			}
			format.Channels = int(binary.LittleEndian.Uint16(wav[body+2:]))
			format.SampleRate = int(binary.LittleEndian.Uint32(wav[body+4:]))
		case "data":
			if format.SampleRate == 0 {
				return realtimeproto.PCMAudio{}, errors.New("WAV 的 data 块在 fmt 块之前")
			}
			return realtimeproto.PCMAudio{Format: format, Data: wav[body : body+size-size%2]}, nil
		}
		offset = body + size + size%2
	}
	return realtimeproto.PCMAudio{}, errors.New("WAV 里没有 data 块")
}
