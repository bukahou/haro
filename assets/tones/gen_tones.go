//go:build ignore

// gen_tones 生成终端内置的提示音（不含人声，任何人都可以随程序分发）：
//
//	offline.wav：由高到低的两声（服务端断开了）
//	online.wav ：由低到高的三声（服务端回来了）
//	sleep.wav  ：轻轻的由高到低两声（对话结束、去休眠了；比断开那声更轻、更低）
//	notice.wav ：明亮的「叮咚」（服务端有事告诉你，例如后台调查查完了）
//	reminder.wav：高低交替的四声（到点的提醒，语音合成用不了时代替说话；比「叮咚」更像闹铃）
//
// 想用自己的声音录的提示音：放进一个目录（offline.wav、online.wav、sleep.wav、notice.wav、reminder.wav，16 bit PCM；
// 缺哪个就用内置的那个），设 HARO_TONES_DIR 指向它。
//
//	go run gen_tones.go   # 在 assets/tones 目录里运行
package main

import (
	"encoding/binary"
	"math"

	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
)

const sampleRate = 24000

// note 是一个音：频率（Hz）、长度（秒）。
type note struct {
	frequency, seconds float64
}

// render 把几个音连起来：每个音带两个泛音，快起慢落，听起来像柔和的「叮」；volume 是 0–1 的音量。
func render(volume float64, notes []note) []int16 {
	var samples []int16
	for _, n := range notes {
		count := int(n.seconds * sampleRate)
		for i := 0; i < count; i++ {
			t := float64(i) / sampleRate
			envelope := math.Min(1, t/0.01) * math.Exp(-4*t/n.seconds)
			value := math.Sin(2*math.Pi*n.frequency*t) + 0.3*math.Sin(4*math.Pi*n.frequency*t) + 0.1*math.Sin(6*math.Pi*n.frequency*t)
			samples = append(samples, int16(value/1.4*envelope*volume*math.MaxInt16))
		}
	}
	return append(samples, make([]int16, sampleRate/10)...) // 结尾留 0.1 秒静音
}

// writeWAV 把单声道样本写成 16 位 PCM 的 WAV 文件。
func writeWAV(path string, samples []int16) {
	data := make([]byte, 2*len(samples))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(sample))
	}
	audio := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: sampleRate, Channels: 1}, Data: data}
	if err := pcmwav.WriteFile(path, audio); err != nil {
		panic(err)
	}
}

func main() {
	writeWAV("offline.wav", render(0.5, []note{{880, 0.28}, {587, 0.45}}))
	writeWAV("online.wav", render(0.5, []note{{523, 0.16}, {659, 0.16}, {880, 0.4}}))
	writeWAV("sleep.wav", render(0.3, []note{{659, 0.25}, {440, 0.55}}))
	writeWAV("notice.wav", render(0.45, []note{{1047, 0.2}, {784, 0.5}}))
	writeWAV("reminder.wav", render(0.5, []note{{988, 0.18}, {1319, 0.18}, {988, 0.18}, {1319, 0.45}}))
}
