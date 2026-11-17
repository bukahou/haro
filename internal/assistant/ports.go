// Package assistant 是 haro 对外唯一的入口：终端循环。
//
// 终端只管输入输出硬件：触发 → 录音 → 取帧 → 交给大脑 → 播放大脑返回的音频。
// 「听懂、思考、说什么」都在大脑（BrainSession）里；终端与大脑之间只传 realtimeproto 事件。
// v1.5 大脑在同一进程（brain.Session）；v3 换成 WebSocket 客户端，本包不改。
//
// 本文件定义终端依赖的各段能力（Go interface，放在使用方一侧），由 cmd/haro 组装注入。
package assistant

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// TurnTrigger 决定一轮对话什么时候开始、什么时候结束说话。
type TurnTrigger interface {
	// WaitForStart 阻塞到用户示意「开始说话」；ctx 取消时返回 ctx.Err()。
	WaitForStart(ctx context.Context) error
	// WaitForStop 阻塞到用户示意「说完了」。
	WaitForStop(ctx context.Context) error
}

// AudioRecorder 开始录一段音，直到调用返回的 Recording.Stop。
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
	// LatestFrame 返回最近一帧（JPEG）与拍摄时刻。常驻实现不等待；按需实现会现抓。
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
	Abort() error          // 立即停止并丢掉缓冲（v2 打断用）
	Played() time.Duration // 已经播出多长（v2 按「用户实际听到的部分」截断历史用）
}

// BrainSession 是终端看到的大脑。
type BrainSession interface {
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Events() <-chan realtimeproto.ServerEvent
}

// Microphone 打开常驻的麦克风流（v2 免按键）。
type Microphone interface {
	OpenStream(ctx context.Context) (MicrophoneStream, error)
}

// MicrophoneStream 持续产出 20ms 一块的 PCM。
type MicrophoneStream interface {
	Chunks() <-chan realtimeproto.PCMAudio // 麦克风停止时关闭
	Close() error
}

// ConversationToggle 决定什么时候进入、退出对话模式（v2.0 是回车；v2.2 换成唤醒词 + 对话窗口）。
type ConversationToggle interface {
	WaitForEnter(ctx context.Context) error
	WaitForLeave(ctx context.Context) error
}

// ManualToggle 是唤醒词模式下的手动开关（回车）：休眠时按下立即唤醒，唤醒时按下立即休眠。
type ManualToggle interface {
	WaitForToggle(ctx context.Context) error
}
