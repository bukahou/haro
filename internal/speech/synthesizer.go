package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/endpoint"
	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// PiperHTTPSynthesizer 调用 Piper 官方 HTTP 服务（python -m piper.http_server）逐句合成。
// 模型常驻在服务里，每句只要几十毫秒；每句一个请求，句子边界清楚、可单独超时。
// 接口：POST /synthesize，JSON {"text": "..."}，返回 WAV。⚠️ 空文本会返回 500，调用方不得传空句。
type PiperHTTPSynthesizer struct {
	BaseURL         string
	SentenceTimeout time.Duration
	Client          *http.Client
}

// maxPiperResponseBytes：Piper 一次回答（整句的 WAV，一次读进内存）最多读这么多，超过就当出错。
// 22.05 kHz 单声道 16 位约 44 KB/秒，16 MB 约 6 分钟，一句话远到不了；定值
const maxPiperResponseBytes = 16 << 20

// NewPiperHTTPSynthesizer 创建合成器。
func NewPiperHTTPSynthesizer(baseURL string, sentenceTimeout time.Duration, client *http.Client) *PiperHTTPSynthesizer {
	return &PiperHTTPSynthesizer{BaseURL: strings.TrimRight(baseURL, "/"), SentenceTimeout: sentenceTimeout, Client: client}
}

// Synthesize 合成一句（整句一次返回；Piper 不支持情绪，忽略 utterance.Emotion，也只会说中文）。
// v3 起 Piper 只作开发用实现，不作备用。
func (p *PiperHTTPSynthesizer) Synthesize(ctx context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		audio, err := p.synthesizeWhole(ctx, utterance.Text)
		yield(audio, err)
	}
}

func (p *PiperHTTPSynthesizer) synthesizeWhole(ctx context.Context, sentence string) (realtimeproto.PCMAudio, error) {
	if strings.TrimSpace(sentence) == "" {
		return realtimeproto.PCMAudio{}, fmt.Errorf("piper: 空句")
	}
	ctx, cancel := context.WithTimeout(ctx, p.SentenceTimeout)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"text": sentence})
	if err != nil {
		return realtimeproto.PCMAudio{}, err
	}
	address, err := endpoint.Join(p.BaseURL, "/synthesize", nil)
	if err != nil {
		return realtimeproto.PCMAudio{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		return realtimeproto.PCMAudio{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return realtimeproto.PCMAudio{}, fmt.Errorf("请求 piper: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPiperResponseBytes+1))
	if err != nil {
		return realtimeproto.PCMAudio{}, fmt.Errorf("读取 piper 响应: %w", err)
	}
	if len(body) > maxPiperResponseBytes {
		return realtimeproto.PCMAudio{}, fmt.Errorf("piper 的回答超过 %d 字节（一句话的 WAV 到不了这么大）", maxPiperResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return realtimeproto.PCMAudio{}, fmt.Errorf("piper 返回 %d: %.200s", resp.StatusCode, body)
	}
	return pcmwav.Decode(body)
}
