package speech

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// GPTSoVITSSynthesizer 调用 GPT-SoVITS 的 api_v2（POST /tts）边合成边产出。
//
// 情绪靠参考录音体现（R-3）：每种情绪对应一段参考录音，没配的情绪用 default。
// 参考录音放在语音合成服务所在的机器上（HARO_TTS_REF_DIR），请求里传「目录 + 文件名」。
// 流式返回的格式：先是一个 WAV 头（带采样率），后面全是裸 PCM。
type GPTSoVITSSynthesizer struct {
	BaseURL         string
	RefDir          string // 参考录音在服务那台机器上的目录
	Voice           GPTSoVITSVoice
	StreamingMode   int // api_v2 的 streaming_mode：0 整句；2 中等（R-2）；3 最快
	SentenceTimeout time.Duration
	Client          *http.Client
}

// GPTSoVITSVoice 是一个声音的全部参考录音（声音档案 JSON，格式见 assets/voices/example.json）。
type GPTSoVITSVoice struct {
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	References  map[string]GPTSoVITSReference `json:"references"` // 情绪名（default / tease / laugh …）→ 参考录音
}

// GPTSoVITSReference 是一段参考录音：文件名 + 录音里说的话（必须逐字一致）+ 录音的语言。
type GPTSoVITSReference struct {
	File string `json:"file"`
	Text string `json:"text"`
	Lang string `json:"lang"`
}

// DefaultReference 是声音档案里平常语气那段参考录音的名字（必须有；没配的情绪也用它）。其余参考录音按情绪名
// （brain.Emotion 的值）取，装配处按情绪清单检查。
const DefaultReference = "default"

// autoLanguage 让 GPT-SoVITS 自己按文字判断用什么语言念（中日英夹杂也能分段念）。
const autoLanguage = "auto"

// LoadGPTSoVITSVoice 读取声音文件并校验：必须有 default，每段录音都要有文件名和文字。
func LoadGPTSoVITSVoice(file string) (GPTSoVITSVoice, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return GPTSoVITSVoice{}, fmt.Errorf("读取声音文件: %w", err)
	}
	var voice GPTSoVITSVoice
	if err := json.Unmarshal(data, &voice); err != nil {
		return GPTSoVITSVoice{}, fmt.Errorf("解析声音文件 %s: %w", file, err)
	}
	if _, ok := voice.References[DefaultReference]; !ok {
		return GPTSoVITSVoice{}, fmt.Errorf("声音文件 %s 缺少 default 参考录音", file)
	}
	for name, ref := range voice.References {
		if ref.File == "" || strings.TrimSpace(ref.Text) == "" || ref.Lang == "" {
			return GPTSoVITSVoice{}, fmt.Errorf("声音文件 %s 的 %s：file、text、lang（参考录音的语言）都必须填", file, name)
		}
	}
	return voice, nil
}

// NewGPTSoVITSSynthesizer 创建合成器。
func NewGPTSoVITSSynthesizer(baseURL, refDir string, voice GPTSoVITSVoice, streamingMode int,
	sentenceTimeout time.Duration, client *http.Client) *GPTSoVITSSynthesizer {
	return &GPTSoVITSSynthesizer{BaseURL: strings.TrimRight(baseURL, "/"), RefDir: refDir, Voice: voice,
		StreamingMode: streamingMode, SentenceTimeout: sentenceTimeout, Client: client}
}

// reference 按情绪选参考录音；没配这种情绪就用 default。
func (g *GPTSoVITSSynthesizer) reference(emotion brain.Emotion) GPTSoVITSReference {
	if emotion != brain.EmotionDefault {
		if ref, ok := g.Voice.References[string(emotion)]; ok {
			return ref
		}
	}
	return g.Voice.References[DefaultReference]
}

type gptSoVITSRequest struct {
	Text            string `json:"text"`
	TextLang        string `json:"text_lang"`
	RefAudioPath    string `json:"ref_audio_path"`
	PromptText      string `json:"prompt_text"`
	PromptLang      string `json:"prompt_lang"`
	TextSplitMethod string `json:"text_split_method"`
	BatchSize       int    `json:"batch_size"`
	MediaType       string `json:"media_type"`
	StreamingMode   int    `json:"streaming_mode"`
}

// readChunkBytes 是每次交给调用方的最大字节数（32 kHz 单声道约 0.25 s）。
const readChunkBytes = 16000

// Synthesize 合成一句，收到一段声音就产出一段。
func (g *GPTSoVITSSynthesizer) Synthesize(ctx context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		if strings.TrimSpace(utterance.Text) == "" {
			yield(realtimeproto.PCMAudio{}, errors.New("gpt-sovits: 空句"))
			return
		}
		ctx, cancel := context.WithTimeout(ctx, g.SentenceTimeout)
		defer cancel()
		resp, err := g.post(ctx, utterance)
		if err != nil {
			yield(realtimeproto.PCMAudio{}, err)
			return
		}
		defer resp.Body.Close()

		reader := bufio.NewReaderSize(resp.Body, readChunkBytes)
		format, err := pcmwav.ReadStreamHeader(reader)
		if err != nil {
			yield(realtimeproto.PCMAudio{}, fmt.Errorf("gpt-sovits 返回的不是 WAV 流: %w", err))
			return
		}
		frame := format.Channels * 2
		buffer := make([]byte, readChunkBytes)
		var carry []byte // 上一段末尾不满一帧（一个采样点）的字节
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				data := append(carry, buffer[:n]...)
				whole := len(data) / frame * frame
				carry = append([]byte(nil), data[whole:]...)
				if whole > 0 && !yield(realtimeproto.PCMAudio{Format: format, Data: append([]byte(nil), data[:whole]...)}, nil) {
					return // 调用方 break：关闭响应体即中止合成
				}
			}
			if errors.Is(readErr, io.EOF) {
				return
			}
			if readErr != nil {
				yield(realtimeproto.PCMAudio{}, fmt.Errorf("读取 gpt-sovits 声音流: %w", readErr))
				return
			}
		}
	}
}

func (g *GPTSoVITSSynthesizer) post(ctx context.Context, utterance brain.Utterance) (*http.Response, error) {
	ref := g.reference(utterance.Emotion)
	payload, err := json.Marshal(gptSoVITSRequest{
		Text: utterance.Text, TextLang: autoLanguage,
		RefAudioPath: path.Join(g.RefDir, ref.File), PromptText: ref.Text, PromptLang: ref.Lang,
		TextSplitMethod: "cut0", // 服务端已经切好句了，不再切（再切会在句中插入停顿）
		BatchSize:       1, MediaType: "wav", StreamingMode: g.StreamingMode,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/tts", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 gpt-sovits（%s）: %w", g.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("gpt-sovits 返回 %d: %s", resp.StatusCode, detail)
	}
	return resp, nil
}
