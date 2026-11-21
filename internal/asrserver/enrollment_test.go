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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// 远程登记声纹：算向量、存盘、立刻生效；补录加进原来的；没登记过不能补录。
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

// 存盘失败是服务端的问题（这里是声纹目录的位置被一个文件占着）：回 500，不说成请求的错。
func TestEnrollmentSaveFailureIsServerError(t *testing.T) {
	embedder := &switchableEmbedder{}
	embedder.current.Store([]float32{1, 0, 0})
	dir := filepath.Join(t.TempDir(), "voiceprints")
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Dependencies{Speakers: &SpeakerCheck{Embedder: embedder, Threshold: 0.5, Dir: dir, Model: "campplus.onnx"},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	clip := base64.StdEncoding.EncodeToString(make([]byte, 3200))
	response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Clips: []string{clip, clip}})
	if response.StatusCode != http.StatusInternalServerError {
		t.Errorf("存盘失败应回 500：%d", response.StatusCode)
	}
}

// gatedEmbedder 先挡住算向量的调用，等 waiting 个请求都到了再一起放行：几次登记同时走到读盘、存盘那一步。
type gatedEmbedder struct {
	waiting int32
	calls   atomic.Int32
	arrived chan struct{} // 第 waiting 次调用时关上
	release chan struct{}
}

func (e *gatedEmbedder) Embed([]float32) ([]float32, error) {
	if e.calls.Add(1) == e.waiting {
		close(e.arrived)
	}
	<-e.release
	return []float32{1, 0, 0}, nil
}

// 同一个人同时补录几次：一次一次地加进去，哪一次的句子都不丢，声纹文件完好，不留临时文件。
func TestConcurrentEnrollmentKeepsEverySentence(t *testing.T) {
	const requests = 16
	dir := t.TempDir()
	if _, err := voiceprint.Save(dir, voiceprint.Voiceprint{Name: "主人", Model: "campplus.onnx",
		Embeddings: [][]float32{{1, 0, 0}, {1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	embedder := &gatedEmbedder{waiting: requests, arrived: make(chan struct{}), release: make(chan struct{})}
	handler := NewHandler(Dependencies{Speakers: &SpeakerCheck{Embedder: embedder, Threshold: 0.5, Dir: dir, Model: "campplus.onnx"},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	clip := base64.StdEncoding.EncodeToString(make([]byte, 3200))

	statuses := make(chan int, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{Append: true, Clips: []string{clip, clip}})
			statuses <- response.StatusCode
		}()
	}
	<-embedder.arrived // 每个请求都在算第一句了：一起放行
	close(embedder.release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("同时补录应都成功：%d", status)
		}
	}
	saved, err := voiceprint.Load(dir, "主人")
	if err != nil || len(saved.Embeddings) != 2+2*requests {
		t.Errorf("每次补录的 2 句都要加进去：应有 %d 句，读到 %d 句（%v）", 2+2*requests, len(saved.Embeddings), err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) > 0 {
		t.Errorf("不应留下临时文件：%v", leftovers)
	}
}

func TestEnrollmentWithoutVoiceprints(t *testing.T) {
	handler := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if response, _ := postEnrollment(t, handler, "主人", realtimeproto.VoiceprintEnrollment{}); response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("没开声纹时应返回 503：%d", response.StatusCode)
	}
}
