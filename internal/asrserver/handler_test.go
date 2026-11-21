package asrserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(context.Context, realtimeproto.PCMAudio) (Transcript, error) {
	return Transcript{Text: "你好。", Language: "zh"}, nil
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func probe(handler http.Handler, path string) int {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code
}

func TestLivezAndReadyz(t *testing.T) {
	handler := NewHandler(Dependencies{Transcriber: fakeTranscriber{}, Logger: discardLogger})
	for _, path := range []string{"/livez", "/readyz"} {
		if code := probe(handler, path); code != http.StatusOK {
			t.Errorf("%s 状态码 %d", path, code)
		}
	}
}

// 识别器出错时 /readyz 是 503（不再固定回 200）；/livez 照旧只看进程。
func TestReadyzReflectsRecognizer(t *testing.T) {
	handler := NewHandler(Dependencies{Transcriber: failingTranscriber{}, Logger: discardLogger})
	if code := probe(handler, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("识别器出错时 /readyz 应是 503：%d", code)
	}
	if code := probe(handler, "/livez"); code != http.StatusOK {
		t.Errorf("/livez 只看进程：%d", code)
	}
}

// stuckTranscriber 一直卡住，直到 release 关上；数被调用了几次。
type stuckTranscriber struct {
	calls   atomic.Int32
	release chan struct{}
}

func (s *stuckTranscriber) Transcribe(context.Context, realtimeproto.PCMAudio) (Transcript, error) {
	s.calls.Add(1)
	<-s.release
	return Transcript{}, nil
}

// 识别器卡住：探测在期限上回 503；几次探测同时来也只识别一次（卡住时不越堆越多）；不卡了就恢复 200。
func TestReadyzWhenRecognizerIsStuck(t *testing.T) {
	transcriber := &stuckTranscriber{release: make(chan struct{})}
	readiness := newRecognizerReadiness(transcriber, discardLogger)
	readiness.timeout = 50 * time.Millisecond
	started := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if code := probe(readiness, "/readyz"); code != http.StatusServiceUnavailable {
				t.Errorf("识别器卡住时应是 503：%d", code)
			}
		})
	}
	wg.Wait()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("卡住时应在期限上回答：等了 %v", elapsed)
	}
	if calls := transcriber.calls.Load(); calls != 1 {
		t.Errorf("同时来的探测应共用一次识别：识别了 %d 次", calls)
	}
	close(transcriber.release)
	deadline := time.Now().Add(5 * time.Second)
	for probe(readiness, "/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("识别器不卡了应恢复就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 识别器出错（不是卡住）也算没就绪，交出的就是识别器的错误。
func TestReadyzRecognizerError(t *testing.T) {
	failure := errors.New("模型出错")
	readiness := newRecognizerReadiness(transcriberFunc(func() error { return failure }), discardLogger)
	if err := readiness.check(context.Background()); !errors.Is(err, failure) {
		t.Errorf("识别器出错应算没就绪：%v", err)
	}
}

// transcriberFunc 每次识别都返回 fail() 的错误。
type transcriberFunc func() error

func (f transcriberFunc) Transcribe(context.Context, realtimeproto.PCMAudio) (Transcript, error) {
	return Transcript{}, f()
}
