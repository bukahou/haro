package assistant_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 服务端的提示（后台调查查完了等）：醒着时语音前端在听，提示音和回答一样防回声；睡着时直接播；
// 主人正说着话时先不响，留到这一轮答完。

// timeline 按先后记下替身服务端收到的控制事件和音箱上发生的事。
type timeline struct {
	mu      sync.Mutex
	entries []string
}

func (l *timeline) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *timeline) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// indexOf 是 entry 第 n 次（从 1 数）出现的位置；没有时为 -1。
func indexOf(entries []string, entry string, n int) int {
	for i, e := range entries {
		if e == entry {
			if n--; n == 0 {
				return i
			}
		}
	}
	return -1
}

// waitFor 等 entry 出现第 n 次，返回那一刻的记录。
func (l *timeline) waitFor(t *testing.T, entry string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries := l.snapshot()
		if indexOf(entries, entry, n) >= 0 {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到第 %d 次 %s：%v", n, entry, entries)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const (
	toneOpened  = "提示音开始"
	toneDrained = "提示音播完"
)

// noticeServer 是替身服务端：事件由测试推送；记下播放开始 / 结束（记进 timeline），数一数收到的音频块里有几块是静音。
type noticeServer struct {
	log    *timeline
	events chan realtimeproto.ServerEvent

	mu           sync.Mutex
	silent, loud int
}

func (s *noticeServer) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	switch e := event.(type) {
	case realtimeproto.InputAudioBufferAppend:
		s.mu.Lock()
		if bytes.Count(e.Audio.Data, []byte{0}) == len(e.Audio.Data) {
			s.silent++
		} else {
			s.loud++
		}
		s.mu.Unlock()
	case realtimeproto.HaroPlaybackStarted, realtimeproto.HaroPlaybackStopped:
		s.log.add(event.Type())
	}
	return nil
}
func (s *noticeServer) Events() <-chan realtimeproto.ServerEvent { return s.events }
func (s *noticeServer) audioCounts() (silent, loud int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.silent, s.loud
}

// waitForAudio 等服务端在 (silent, loud) 之后又收到至少 n 块满足 counted 的音频。
func (s *noticeServer) waitForAudio(t *testing.T, what string, n int, counted func(silent, loud int) int) {
	t.Helper()
	start := counted(s.audioCounts())
	deadline := time.Now().Add(3 * time.Second)
	for counted(s.audioCounts())-start < n {
		if time.Now().After(deadline) {
			t.Fatalf("等不到%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func silentChunks(silent, _ int) int { return silent }
func loudChunks(_, loud int) int     { return loud }

// heldSpeaker 是替身音箱：每播一段，要等测试往 release 里放一个信号才算播完（模拟声音还在响）。
type heldSpeaker struct {
	log     *timeline
	release chan struct{}
}

func (s *heldSpeaker) OpenStream(context.Context, realtimeproto.AudioFormat) (assistant.AudioStream, error) {
	s.log.add(toneOpened)
	return s, nil
}
func (s *heldSpeaker) Write([]byte) error { return nil }
func (s *heldSpeaker) Drain() error {
	select {
	case <-s.release:
	case <-time.After(3 * time.Second): // 测试出错时不一直挂着
	}
	s.log.add(toneDrained)
	return nil
}
func (s *heldSpeaker) Abort() error          { return nil }
func (s *heldSpeaker) Played() time.Duration { return 0 }

// waitingManual 是没有键盘的终端：一直等到取消。
type waitingManual struct{}

func (waitingManual) WaitForToggle(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

const echoGuard = 30 * time.Millisecond

// startNoticeAssistant 起一个从休眠开始的终端，返回替身服务端、替身音箱。
func startNoticeAssistant(t *testing.T, bargeIn bool) (*noticeServer, *heldSpeaker) {
	t.Helper()
	log := &timeline{}
	server := &noticeServer{log: log, events: make(chan realtimeproto.ServerEvent, 16)}
	speaker := &heldSpeaker{log: log, release: make(chan struct{}, 4)}
	haro := assistant.NewAssistant(assistant.Dependencies{
		Manual: waitingManual{}, Microphone: loudMicrophone{}, Speaker: speaker, Brain: server,
	}, assistant.Options{TurnDetection: realtimeproto.TurnDetection{Type: "server_vad"}, EchoGuard: echoGuard, BargeIn: bargeIn,
		NoticeTone: pcm("[叮咚]"), WakeWord: assistant.WakeWordOptions{Phrase: "哈罗哈罗", IdleTimeout: time.Minute}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = haro.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return server, speaker
}

var researchDone = realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone}

// 可以打断时：醒着播提示音前告诉语音前端「正在播放」（它只听唤醒词），播完再等余音散掉才说「播完了」；睡着时直接播。
func TestNoticeWhileAwakeAnnouncesPlayback(t *testing.T) {
	server, speaker := startNoticeAssistant(t, true)
	log := server.log

	server.events <- researchDone // 睡着
	log.waitFor(t, toneOpened, 1)
	speaker.release <- struct{}{}
	if entries := log.waitFor(t, toneDrained, 1); slices.Contains(entries, "haro.playback.started") {
		t.Fatalf("睡着时的提示音不该告诉语音前端正在播放：%v", entries)
	}

	server.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}
	server.events <- researchDone // 醒着
	entries := log.waitFor(t, toneOpened, 2)
	if started := indexOf(entries, "haro.playback.started", 1); started < 0 || started > indexOf(entries, toneOpened, 2) {
		t.Fatalf("醒着时应在提示音响之前告诉语音前端正在播放：%v", entries)
	}
	releasedAt := time.Now()
	speaker.release <- struct{}{}
	entries = log.waitFor(t, "haro.playback.stopped", 1)
	if time.Since(releasedAt) < echoGuard || indexOf(entries, "haro.playback.stopped", 1) < indexOf(entries, toneDrained, 2) {
		t.Errorf("应在提示音播完、再等 %v 余音之后才说播完了：%v", echoGuard, entries)
	}
}

// 不能打断时（半双工）：醒着播提示音期间上传静音，播完、等余音散掉后恢复；睡着时照常上传。
func TestNoticeWhileAwakeMutesUpload(t *testing.T) {
	server, speaker := startNoticeAssistant(t, false)
	log := server.log

	server.events <- researchDone // 睡着
	log.waitFor(t, toneOpened, 1)
	server.waitForAudio(t, "睡着时提示音响着也照常上传", 5, loudChunks)
	if silent, _ := server.audioCounts(); silent != 0 {
		t.Fatalf("睡着时不该上传静音：%d 块", silent)
	}
	speaker.release <- struct{}{}
	log.waitFor(t, toneDrained, 1)

	server.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}
	server.events <- researchDone // 醒着
	log.waitFor(t, toneOpened, 2)
	server.waitForAudio(t, "醒着时提示音响着要上传静音", 5, silentChunks)
	if _, loud := server.audioCounts(); loud == 0 {
		t.Fatal("测试前提：之前应上传过声音")
	}
	speaker.release <- struct{}{}
	log.waitFor(t, toneDrained, 2)
	server.waitForAudio(t, "提示音播完、余音散掉后恢复上传", 1, loudChunks)
	if entries := log.snapshot(); slices.Contains(entries, "haro.playback.started") {
		t.Errorf("不能打断时不发播放开始 / 结束：%v", entries)
	}
}

// 主人正说着话时来了提示：先不响（会混进这句话、防回声又会截断它），这一轮答完再响。
func TestNoticeWaitsUntilTheOwnerIsAnswered(t *testing.T) {
	log := &timeline{}
	server := &noticeServer{log: log, events: make(chan realtimeproto.ServerEvent, 16)}
	speaker := &recordingSpeaker{}
	haro := assistant.NewAssistant(assistant.Dependencies{
		Manual: waitingManual{}, Microphone: loudMicrophone{}, Speaker: speaker, Brain: server,
	}, assistant.Options{TurnDetection: realtimeproto.TurnDetection{Type: "server_vad"}, EchoGuard: echoGuard,
		NoticeTone: pcm("[叮咚]"), WakeWord: assistant.WakeWordOptions{Phrase: "哈罗哈罗", IdleTimeout: time.Minute}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = haro.Run(ctx) }()

	for _, event := range []realtimeproto.ServerEvent{
		realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"},
		realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_1"},
		researchDone, // 主人还在说
		realtimeproto.InputAudioBufferSpeechStopped{ItemID: "item_1"},
		realtimeproto.ResponseOutputAudioDelta{Audio: pcm("回答。")},
		realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted},
	} {
		server.events <- event
	}
	waitForPlayed(t, speaker, "回答。[叮咚]", time.Now().Add(3*time.Second), "提示应在这一轮答完之后才响")
}
