// Package assistant 是 haro 对外唯一的入口：终端循环。
//
// 终端只管输入输出硬件：麦克风常开上传 → 服务端判断说完 → 取帧 → 播放服务端返回的音频。
// 「听懂、思考、说什么」都在服务端（BrainSession）里；终端与服务端之间只传 realtimeproto 事件。
// 服务端可以在同一进程（brain.Session，测试里这样用），也可以经 WebSocket 连接（brainclient），本包不区分。
//
// 本文件定义终端依赖的各段能力（Go interface，放在使用方一侧），由 cmd/haro 组装注入。
package assistant

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// AudioRecorder 开始录一段音，直到调用返回的 Recording.Stop（声纹登记用）。
type AudioRecorder interface {
	StartRecording(ctx context.Context) (Recording, error)
}

// Recording 是一段进行中的录音。
type Recording interface {
	// Stop 结束录音，返回录到的 PCM。
	Stop() (realtimeproto.PCMAudio, error)
}

// FrameSource 提供摄像头画面。
type FrameSource interface {
	// LatestFrame 现拍一帧（JPEG），返回画面和拍摄时刻。
	LatestFrame(ctx context.Context) ([]byte, time.Time, error)
}

// AudioSink 打开播放流。一轮回答的所有句子写进同一条流。
type AudioSink interface {
	OpenStream(ctx context.Context, format realtimeproto.AudioFormat) (AudioStream, error)
}

// AudioStream 是一条进行中的播放流。
type AudioStream interface {
	Write(pcm []byte) error
	Drain() error          // 等缓冲里的声音全部播完
	Abort() error          // 立即停止并丢掉缓冲（打断用）
	Played() time.Duration // 已经播出多长（按「用户实际听到的部分」截断历史用）
}

// BrainSession 是终端看到的服务端。
type BrainSession interface {
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Events() <-chan realtimeproto.ServerEvent
}

// Microphone 打开常驻的麦克风流。
type Microphone interface {
	OpenStream(ctx context.Context) (MicrophoneStream, error)
}

// MicrophoneStream 持续产出 20ms 一块的 PCM。
type MicrophoneStream interface {
	Chunks() <-chan realtimeproto.PCMAudio // 麦克风停止时关闭
	Close() error
}

// ManualToggle 是唤醒词模式下的手动开关（回车）：休眠时按下立即唤醒，唤醒时按下立即休眠。
type ManualToggle interface {
	WaitForToggle(ctx context.Context) error
}
