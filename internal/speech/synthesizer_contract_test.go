package speech

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 语音合成（brain.SpeechSynthesizer）的契约：两种实现（GPT-SoVITS、Piper）跑同一组用例。替身服务照各自的接口回话，
// 用例只看 SpeechSynthesizer 交出来的东西。

// synthesisBackend 是一种语音合成实现：怎么建它、它的服务在哪个路径、正常时怎么回一句。
type synthesisBackend struct {
	name  string
	path  string
	build func(baseURL string, timeout time.Duration, client *http.Client) brain.SpeechSynthesizer
	reply func(t *testing.T, w http.ResponseWriter, audio realtimeproto.PCMAudio) // 正常时回这段声音
}

var synthesisBackends = []synthesisBackend{
	{
		name: "GPT-SoVITS", path: "/tts",
		build: func(baseURL string, timeout time.Duration, client *http.Client) brain.SpeechSynthesizer {
			return NewGPTSoVITSSynthesizer(baseURL, "/refs", testVoice, 2, timeout, client)
		},
		reply: func(_ *testing.T, w http.ResponseWriter, audio realtimeproto.PCMAudio) { // 流式：WAV 头 + 裸 PCM
			_, _ = w.Write(streamingWAVHeader(audio.Format.SampleRate, audio.Format.Channels))
			_, _ = w.Write(audio.Data)
		},
	},
	{
		name: "Piper", path: "/synthesize",
		build: func(baseURL string, timeout time.Duration, client *http.Client) brain.SpeechSynthesizer {
			return NewPiperHTTPSynthesizer(baseURL, timeout, client)
		},
		reply: func(t *testing.T, w http.ResponseWriter, audio realtimeproto.PCMAudio) { // 整句一个 WAV 文件
			_, _ = w.Write(wavFile(t, audio))
		},
	},
}

// wavFile 是这段声音的完整 WAV 文件内容。
func wavFile(t *testing.T, audio realtimeproto.PCMAudio) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reply.wav")
	if err := pcmwav.WriteFile(path, audio); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// synthesize 合成一句，交出拼起来的声音和遇到的第一个错误。
func synthesize(synthesizer brain.SpeechSynthesizer, text string) (realtimeproto.PCMAudio, error) {
	var whole realtimeproto.PCMAudio
	for audio, err := range synthesizer.Synthesize(context.Background(), brain.Utterance{Text: text}) {
		if err != nil {
			return whole, err
		}
		whole.Format = audio.Format
		whole.Data = append(whole.Data, audio.Data...)
	}
	return whole, nil
}

func TestSynthesizerContract(t *testing.T) {
	sentence := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1},
		Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	for _, backend := range synthesisBackends {
		t.Run(backend.name, func(t *testing.T) {
			var requests atomic.Int32
			// serve 起一个替身服务，每个请求交给 handle
			serve := func(handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != backend.path {
						t.Errorf("请求 %s %s，应是 POST %s", r.Method, r.URL.Path, backend.path)
					}
					handle(w, r)
				}))
				t.Cleanup(server.Close)
				return server
			}

			t.Run("正常：交出服务回的声音和格式", func(t *testing.T) {
				server := serve(func(w http.ResponseWriter, _ *http.Request) { backend.reply(t, w, sentence) })
				audio, err := synthesize(backend.build(server.URL, 5*time.Second, server.Client()), "你好。")
				if err != nil || audio.Format != sentence.Format || !bytes.Equal(audio.Data, sentence.Data) {
					t.Errorf("应原样交出声音：%+v %v", audio, err)
				}
			})
			t.Run("空句：报错，不去请求服务", func(t *testing.T) {
				server := serve(func(w http.ResponseWriter, _ *http.Request) { backend.reply(t, w, sentence) })
				before := requests.Load()
				if _, err := synthesize(backend.build(server.URL, 5*time.Second, server.Client()), "  "); err == nil {
					t.Error("空句应报错")
				}
				if requests.Load() != before {
					t.Error("空句不应去请求服务")
				}
			})
			t.Run("服务报错：报错，不交声音", func(t *testing.T) {
				server := serve(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "模型没加载", http.StatusInternalServerError)
				})
				if audio, err := synthesize(backend.build(server.URL, 5*time.Second, server.Client()), "你好。"); err == nil || len(audio.Data) > 0 {
					t.Errorf("服务报错应报错：%+v %v", audio, err)
				}
			})
			t.Run("超时：在每句的期限上报错", func(t *testing.T) {
				release := make(chan struct{})
				defer close(release) // 用例结束后替身服务才回，不拖住关服务
				server := serve(func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-release:
					case <-r.Context().Done():
					}
				})
				started := time.Now()
				if _, err := synthesize(backend.build(server.URL, 50*time.Millisecond, server.Client()), "你好。"); err == nil {
					t.Error("超时应报错")
				}
				if elapsed := time.Since(started); elapsed > 2*time.Second {
					t.Errorf("应在每句的期限上结束：等了 %v", elapsed)
				}
			})
			t.Run("回的不是 WAV：报错", func(t *testing.T) {
				server := serve(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"message":"这不是声音"}`))
				})
				if audio, err := synthesize(backend.build(server.URL, 5*time.Second, server.Client()), "你好。"); err == nil || len(audio.Data) > 0 {
					t.Errorf("回的不是 WAV 应报错：%+v %v", audio, err)
				}
			})
		})
	}
}

// Piper 整句一次读进内存：回答超过上限（maxPiperResponseBytes）就报错，哪怕它是一个完整的 WAV，也不全读进来。
func TestPiperResponseLimit(t *testing.T) {
	huge := wavFile(t, realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1},
		Data: make([]byte, maxPiperResponseBytes)}) // 加上 WAV 头就超过上限
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(huge) }))
	defer server.Close()
	synthesizer := NewPiperHTTPSynthesizer(server.URL, 10*time.Second, server.Client())
	if _, err := synthesize(synthesizer, "你好。"); err == nil {
		t.Error("回答超过上限应报错")
	}
}
