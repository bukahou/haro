// Package speech 负责语音与文字的互转：recognizer（语音 → 文字）、synthesizer（文字 → 语音）。
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

// VoiceAPIRecognizer 调用外部的 Whisper 转写服务（whisper.cpp 兼容的 HTTP 接口）做语音转文字。
// 接口：POST /v1/transcriptions，multipart 字段 audio，返回 {"text": "..."}。
// ⚠️ whisper.cpp 的服务同一时间只处理一个请求；它忙时这里会排队。
type VoiceAPIRecognizer struct {
	BaseURL string
	Client  *http.Client
}

// NewVoiceAPIRecognizer 创建识别器。
func NewVoiceAPIRecognizer(baseURL string, client *http.Client) *VoiceAPIRecognizer {
	return &VoiceAPIRecognizer{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

// Recognize 把 PCM 包成 WAV 上传，返回识别出的文字（去掉首尾空白）。
func (r *VoiceAPIRecognizer) Recognize(ctx context.Context, audio realtimeproto.PCMAudio) (string, error) {
	body, contentType, err := buildAudioUpload(EncodeWAV(audio))
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/v1/transcriptions", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 voice-api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("voice-api 返回 %d: %s", resp.StatusCode, detail)
	}
	var transcription struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&transcription); err != nil {
		return "", fmt.Errorf("解析 voice-api 响应: %w", err)
	}
	return strings.TrimSpace(transcription.Text), nil
}

func buildAudioUpload(wav []byte) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("audio", "speech.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}
