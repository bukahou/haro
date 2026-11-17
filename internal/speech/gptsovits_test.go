package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// streamingWAVHeader 仿照 api_v2 的 wave_header_chunk：标准 44 字节头，data 长度为 0。
func streamingWAVHeader(rate, channels int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*channels*2))
	binary.Write(&b, binary.LittleEndian, uint16(channels*2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(0))
	return b.Bytes()
}

var testVoice = GPTSoVITSVoice{Name: "test", References: map[string]GPTSoVITSReference{
	"default": {File: "ref-tease.wav", Text: "你问我冰箱里有什么？", Lang: "zh"},
	"laugh":   {File: "ref-laugh.wav", Text: "哈哈哈，好问题。"},
}}

func TestGPTSoVITSStreamsChunks(t *testing.T) {
	var got gptSoVITSRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tts" || r.Method != http.MethodPost {
			t.Errorf("请求 %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		flusher := w.(http.Flusher)
		w.Write(streamingWAVHeader(32000, 1))
		flusher.Flush()
		w.Write([]byte{1, 2, 3}) // 故意切在一个采样点中间
		flusher.Flush()
		w.Write([]byte{4, 5, 6, 7, 8})
	}))
	defer server.Close()

	synth := NewGPTSoVITSSynthesizer(server.URL+"/", "/Users/haro/refs", testVoice, 2, 5*time.Second, server.Client())
	var pcm []byte
	for audio, err := range synth.Synthesize(context.Background(),
		brain.Utterance{Text: "哈哈，又穿反了。", Emotion: brain.EmotionLaugh, Language: brain.LanguageChinese}) {
		if err != nil {
			t.Fatal(err)
		}
		if audio.Format.SampleRate != 32000 || audio.Format.Channels != 1 || len(audio.Data)%2 != 0 {
			t.Fatalf("格式不对或不满一帧：%+v，%d 字节", audio.Format, len(audio.Data))
		}
		pcm = append(pcm, audio.Data...)
	}
	if !bytes.Equal(pcm, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("PCM 应原样拼接，得到 %v", pcm)
	}
	if got.RefAudioPath != "/Users/haro/refs/ref-laugh.wav" || got.PromptText != "哈哈哈，好问题。" ||
		got.PromptLang != "zh" || got.TextLang != "zh" || got.StreamingMode != 2 || got.MediaType != "wav" ||
		got.TextSplitMethod != "cut0" {
		t.Errorf("请求体 %+v", got)
	}
}

func TestGPTSoVITSFallsBackToDefaultReference(t *testing.T) {
	synth := NewGPTSoVITSSynthesizer("http://x", "/refs", testVoice, 2, time.Second, http.DefaultClient)
	if ref := synth.reference(brain.EmotionWhisper); ref.File != "ref-tease.wav" { // 没配「轻声」⇒ default
		t.Errorf("没配的情绪应用 default，得到 %+v", ref)
	}
	if ref := synth.reference(brain.EmotionDefault); ref.File != "ref-tease.wav" {
		t.Errorf("平常应用 default，得到 %+v", ref)
	}
}

func TestGPTSoVITSReportsServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"text_lang: xx is not supported"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	synth := NewGPTSoVITSSynthesizer(server.URL, "/refs", testVoice, 2, time.Second, server.Client())
	for _, err := range synth.Synthesize(context.Background(), brain.Utterance{Text: "你好。"}) {
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("应返回 400 错误，得到 %v", err)
		}
		return
	}
	t.Error("应该产出一个错误")
}

func TestLoadGPTSoVITSVoice(t *testing.T) {
	// 仓库里的示例声音档案必须能通过校验（正式的声音档案不在公开仓库里，由部署方提供）
	voice, err := LoadGPTSoVITSVoice(filepath.Join("..", "..", "assets", "voices", "example.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, emotion := range []brain.Emotion{brain.EmotionTease, brain.EmotionLaugh, brain.EmotionExcited,
		brain.EmotionGentle, brain.EmotionWhisper} {
		if _, ok := voice.References[string(emotion)]; !ok {
			t.Errorf("example.json 缺少情绪 %s", emotion)
		}
	}
	// 缺 default 要报错
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"name":"x","references":{"laugh":{"file":"a.wav","text":"哈"}}}`), 0o600)
	if _, err := LoadGPTSoVITSVoice(bad); err == nil {
		t.Error("缺 default 应报错")
	}
}
