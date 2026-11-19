package asrserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/voiceprint"
)

func postEnrollment(t *testing.T, handler http.Handler, name string, request realtimeproto.VoiceprintEnrollment) (*http.Response, realtimeproto.VoiceprintEnrollmentResult) {
	t.Helper()
	body, _ := json.Marshal(request)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, realtimeproto.VoiceprintPath+name, bytes.NewReader(body)))
	var result realtimeproto.VoiceprintEnrollmentResult
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	return recorder.Result(), result
}

// v4 C-3：远程登记声纹——算向量、存盘、立刻生效；补录加进原来的；没登记过不能补录。
func TestEnrollmentSavesAndTakesEffect(t *testing.T) {
	embedder := &switchableEmbedder{}
	embedder.current.Store([]float32{1, 0, 0})
	dir := t.TempDir()
	speakers := &SpeakerCheck{Embedder: embedder, Threshold: 0.5, Dir: dir, Model: "campplus.onnx"}
	handler := NewHandler(Dependencies{Speakers: speakers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	clip := base64.StdEncoding.EncodeToString(make([]byte, 3200))
	clips := []string{clip, clip, clip}

	if response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Append: true, Clips: clips}); response.StatusCode != http.StatusBadRequest {
		t.Errorf("没登记过就补录应拒绝：%d", response.StatusCode)
	}
	if speakers.enabled() {
		t.Fatal("登记之前不该开始比对")
	}
	response, result := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Clips: clips})
	if response.StatusCode != http.StatusOK || result.Sentences != 3 || len(result.Pairwise) != 3 || result.Pairwise[0].Score < 0.99 {
		t.Fatalf("登记：%d %+v", response.StatusCode, result)
	}
	if !speakers.enabled() || len(result.Speakers) != 1 {
		t.Error("登记后应立刻生效（不用重启）")
	}
	if saved, err := voiceprint.Load(dir, "主人"); err != nil || saved.Model != "campplus.onnx" || len(saved.Embeddings) != 3 {
		t.Errorf("应存进声纹目录：%+v %v", saved, err)
	}
	_, result = postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Append: true, Clips: clips[:2]})
	if result.Sentences != 5 || result.BestVsExisting == nil || *result.BestVsExisting < 0.99 {
		t.Errorf("补录应加进原来的、报告和原来的相似度：%+v", result)
	}
	if response, _ := postEnrollment(t, handler, url.PathEscape("名 字"), realtimeproto.VoiceprintEnrollment{Clips: clips}); response.StatusCode != http.StatusBadRequest {
		t.Errorf("不合法的名字应拒绝：%d", response.StatusCode)
	}
	if response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Clips: clips[:1]}); response.StatusCode != http.StatusBadRequest {
		t.Errorf("句子太少应拒绝：%d", response.StatusCode)
	}
}

func TestEnrollmentWithoutVoiceprints(t *testing.T) {
	handler := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{}); response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("没开声纹时应返回 503：%d", response.StatusCode)
	}
}
