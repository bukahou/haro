package brainclient_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainclient"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

var (
	mic16k   = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}
	voice32k = realtimeproto.AudioFormat{SampleRate: 32000, Channels: 1}
	quiet    = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type fixedRecognizer struct{ text string }

func (r fixedRecognizer) Recognize(context.Context, realtimeproto.PCMAudio) (string, error) {
	return r.text, nil
}

// slowModel 先吐 tokens；hang 为 true 时吐完不结束（模拟还在生成），直到 ctx 取消。
type slowModel struct {
	tokens []string
	hang   bool
}

func (m slowModel) StreamReply(ctx context.Context, _ []brain.ChatMessage, _ []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range m.tokens {
			if !yield(brain.ReplyDelta{Text: token}, nil) {
				return
			}
		}
		if m.hang {
			<-ctx.Done()
			yield(brain.ReplyDelta{}, ctx.Err())
		}
	}
}
func (slowModel) Warmup(context.Context) error { return nil }

// chunkySynthesizer 每句分两段产出 32 kHz 的声音（像 GPT-SoVITS 流式那样）。
type chunkySynthesizer struct{}

func (chunkySynthesizer) Synthesize(_ context.Context, u brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		half := []byte(u.Text)
		if yield(realtimeproto.PCMAudio{Format: voice32k, Data: half[:len(half)/2*2/2*2]}, nil) {
			yield(realtimeproto.PCMAudio{Format: voice32k, Data: []byte("尾巴")}, nil)
		}
	}
}

func newServer(model brain.ChatModel) *httptest.Server {
	return httptest.NewServer(brainserver.NewHandler(func() brainserver.Session {
		return brain.NewSession(brain.Components{
			Recognizer: fixedRecognizer{text: "讲个冷笑话"}, Model: model, Synthesizer: chunkySynthesizer{},
			NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
		}, brain.Options{MaxHistoryTurn: 6, Instructions: "你是测试助手"}, quiet)
	}, quiet))
}

func askOverWebSocket(t *testing.T, client *brainclient.Client) {
	t.Helper()
	ctx := context.Background()
	for _, event := range []realtimeproto.ClientEvent{
		realtimeproto.SessionUpdate{}, // 按键模式；输入格式由客户端补上
		realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: mic16k, Data: make([]byte, 32000)}},
		realtimeproto.InputAudioBufferCommit{},
		realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
			Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: []byte{0xFF, 0xD8}}}}},
		realtimeproto.ResponseCreate{},
	} {
		if err := client.Send(ctx, event); err != nil {
			t.Fatalf("发送 %s: %v", event.Type(), err)
		}
	}
}

func collectUntilDone(t *testing.T, client *brainclient.Client) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-client.Events():
			events = append(events, event)
			if _, done := event.(realtimeproto.ResponseDone); done {
				return events
			}
		case <-timeout:
			t.Fatalf("等 response.done 超时，已收到 %v", events)
		}
	}
}

// 一轮完整的对话走 WebSocket：识别结果、逐句文字、分段的 32 kHz 声音、response.done 都原样到达终端。
func TestConversationOverWebSocket(t *testing.T) {
	server := newServer(slowModel{tokens: []string{"[调侃]为什么数学老师", "离婚了？", "因为解不开。"}})
	defer server.Close()
	client, err := brainclient.Dial(context.Background(), server.URL, mic16k, time.Second, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	askOverWebSocket(t, client)
	events := collectUntilDone(t, client)

	var transcript, spoken strings.Builder
	audioDeltas := 0
	for _, event := range events {
		switch e := event.(type) {
		case realtimeproto.InputAudioTranscriptionCompleted:
			if e.Transcript != "讲个冷笑话" {
				t.Errorf("识别结果 %q", e.Transcript)
			}
		case realtimeproto.ResponseOutputAudioTranscriptDelta:
			transcript.WriteString(e.Delta)
		case realtimeproto.ResponseOutputAudioDelta:
			if e.Audio.Format != voice32k {
				t.Errorf("音频格式应原样带过来：%+v", e.Audio.Format)
			}
			audioDeltas++
			spoken.Write(e.Audio.Data)
		case realtimeproto.Error:
			t.Errorf("不应出错：%s", e.Message)
		}
	}
	if transcript.String() != "为什么数学老师离婚了？因为解不开。" {
		t.Errorf("文字（不带情绪标记）= %q", transcript.String())
	}
	if audioDeltas < 4 { // 两句，每句两段
		t.Errorf("声音应分段到达，只收到 %d 段", audioDeltas)
	}
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("response.done = %+v", done)
	}
}

// trackingListener 记下每条底层连接，killAll 一次性关掉（模拟服务端进程挂掉；httptest 管不到已升级成 WebSocket 的连接）。
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *trackingListener) killAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}

// 回答到一半服务端断了：终端收到 error + 失败的 response.done（不会一直等），之后 Send 返回 ErrDisconnected。
func TestDisconnectDuringResponse(t *testing.T) {
	server := newServer(slowModel{tokens: []string{"说到一半"}, hang: true})
	server.Close() // 只借用它的 handler，改用自己的监听
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &trackingListener{Listener: raw}
	httpServer := &http.Server{Handler: server.Config.Handler}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	client, err := brainclient.Dial(context.Background(), "http://"+raw.Addr().String(), mic16k, time.Second, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	askOverWebSocket(t, client)
	time.Sleep(100 * time.Millisecond)
	listener.killAll()

	events := collectUntilDone(t, client)
	sawError := false
	for _, event := range events {
		if _, ok := event.(realtimeproto.Error); ok {
			sawError = true
		}
	}
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if !sawError || done.Status != realtimeproto.StatusFailed || done.Reason != "brain_disconnected" {
		t.Errorf("应收到 error 和失败的 response.done：%v", events)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() 应该关闭")
	}
	if err := client.Send(context.Background(), realtimeproto.ResponseCreate{}); !errors.Is(err, brainclient.ErrDisconnected) {
		t.Errorf("断开后 Send 应返回 ErrDisconnected，得到 %v", err)
	}
}

// 服务端一直没起来：等满 wait 后给出明确的错误。
func TestDialGivesUpAfterWait(t *testing.T) {
	server := newServer(slowModel{})
	url := server.URL
	server.Close() // 地址存在但没人监听
	started := time.Now()
	_, err := brainclient.Dial(context.Background(), url, mic16k, 1500*time.Millisecond, quiet)
	if err == nil || !strings.Contains(err.Error(), "连不上服务端") {
		t.Fatalf("应返回「连不上服务端」，得到 %v", err)
	}
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Errorf("应该重试到等待时间用完，只等了 %s", elapsed)
	}
}

// 音频格式和声明的不一致：直接拒绝（避免服务端按错的采样率去识别）。
func TestRejectsMismatchedAudioFormat(t *testing.T) {
	server := newServer(slowModel{})
	defer server.Close()
	client, err := brainclient.Dial(context.Background(), server.URL, mic16k, time.Second, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Send(context.Background(), realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte{1, 2}}})
	if err == nil {
		t.Error("格式不一致应报错")
	}
}
