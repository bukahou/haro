package brainserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 同时的对话会话数到了上限：新连接在握手之前就回 503 和错误码，不建会话；有会话结束、腾出名额后又能连上。
func TestSessionLimit(t *testing.T) {
	session := newFakeSession()
	server := newServer(t, session, brainserver.Options{MaxSessions: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, wsURL(server), &websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol}})
	}
	first, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	second, response, err := dial()
	if err == nil {
		second.CloseNow()
		t.Fatal("会话数到了上限时应拒绝新连接")
	}
	var body realtimeproto.HTTPError
	if response == nil || response.StatusCode != http.StatusServiceUnavailable ||
		json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&body) != nil || body.Error.Code != realtimeproto.CodeTooManySessions {
		t.Fatalf("应回 503 和错误码 %s：%v %+v %v", realtimeproto.CodeTooManySessions, response, body, err)
	}
	if err := first.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	for { // 服务端收尾第一条会话之后名额才腾出来
		third, _, err := dial()
		if err == nil {
			third.CloseNow()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("第一条会话结束后应能再连上：%v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// 会话拒绝了 session.update（声明的输入格式不对）时整条不生效：之后的音频不按那个格式解，照样报「没声明格式」；
// 会话收下了才按它声明的格式解。
func TestRejectedSessionUpdateKeepsInputFormat(t *testing.T) {
	session := newFakeSession()
	server := newServer(t, session, brainserver.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server), &websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	update := func(rate int) string {
		return fmt.Sprintf(`{"type":"session.update","session":{"type":"realtime","audio":{"input":{"format":{"type":"audio/pcm","rate":%d},`+
			`"turn_detection":{"type":"server_vad"}}}}}`, rate)
	}
	const appendAudio = `{"type":"input_audio_buffer.append","audio":"AQI="}`
	expectError := func(send string, want realtimeproto.ErrorCode) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(send)); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if failure, ok := event.(realtimeproto.Error); err != nil || !ok || failure.Code != want {
			t.Errorf("发 %s 应收到错误码 %s，得到 %s", send, want, data)
		}
	}
	expectError(update(24000), realtimeproto.CodeUnsupportedAudioFormat)
	expectError(appendAudio, realtimeproto.CodeInputFormatRequired)

	for _, send := range []string{update(16000), appendAudio} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(send)); err != nil {
			t.Fatal(err)
		}
	}
	for {
		select {
		case event := <-session.received:
			if audio, ok := event.(realtimeproto.InputAudioBufferAppend); ok {
				if audio.Audio.Format != realtimeproto.TranscriptionFormat {
					t.Errorf("收下 16 kHz 的 session.update 之后应按它解音频：%+v", audio.Audio.Format)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("收下 session.update 之后的音频应交给会话")
		}
	}
}
