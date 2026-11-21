// Package tones 是终端在服务端连不上时自己播的提示音（v4 第 1 步 1-B）：服务端断开时没有语音合成可用，
// 所以提示音是提前做好的。
//
// 编进程序里的是不含人声的提示音（gen_tones.go 生成：断开 = 由高到低两声，回来 = 由低到高三声，
// 去休眠 = 轻轻的由高到低两声，提示 = 明亮的「叮咚」）。
// 想用自己的声音录的提示音（例如「我和服务端断开了」「我回来了」「我去睡觉了」）：把 offline.wav、online.wav、
// sleep.wav、notice.wav 放进一个目录，设 HARO_TONES_DIR 指向它（Load；缺哪个就用内置的那个）。
package tones

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
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
	Offline realtimeproto.PCMAudio // 服务端断开了
	Online  realtimeproto.PCMAudio // 服务端回来了
	Sleep   realtimeproto.PCMAudio // 对话结束、去休眠了
	Notice  realtimeproto.PCMAudio // 服务端的提示（例如后台调查查完了）：叫醒问就行
}

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
		audio, err := pcmwav.Decode(data)
		if err != nil {
			return tones, fmt.Errorf("提示音 %s: %w", tone.file, err)
		}
		*tone.target = audio
	}
	return tones, nil
}
