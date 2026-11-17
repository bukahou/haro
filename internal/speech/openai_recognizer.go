package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"haro/internal/realtimeproto"
)

// OpenAITranscriptionRecognizer 调用任何 OpenAI 兼容的 POST /v1/audio/transcriptions 做语音转文字：
// haro-asr（SenseVoice）、vLLM（Qwen3-ASR）、speaches、whisper.cpp 等都可以。换引擎只改地址。
type OpenAITranscriptionRecognizer struct {
	BaseURL   string
	ModelName string // 请求里的 model 字段；haro-asr 会忽略
	Client    *http.Client
}

// NewOpenAITranscriptionRecognizer 创建识别客户端。
func NewOpenAITranscriptionRecognizer(baseURL, modelName string, client *http.Client) *OpenAITranscriptionRecognizer {
	return &OpenAITranscriptionRecognizer{BaseURL: strings.TrimRight(baseURL, "/"), ModelName: modelName, Client: client}
}

// Recognize 把 PCM 包成 WAV 上传，返回识别出的文字。
func (r *OpenAITranscriptionRecognizer) Recognize(ctx context.Context, audio realtimeproto.PCMAudio) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "speech.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(EncodeWAV(audio)); err != nil {
		return "", err
	}
	_ = writer.WriteField("model", r.ModelName)
	_ = writer.WriteField("response_format", "json")
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求识别服务: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("识别服务返回 %d: %s", resp.StatusCode, detail)
	}
	var transcription struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&transcription); err != nil {
		return "", fmt.Errorf("解析识别服务响应: %w", err)
	}
	return strings.TrimSpace(transcription.Text), nil
}
