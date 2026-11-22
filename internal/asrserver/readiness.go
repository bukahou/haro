package asrserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/bukahou/haro/internal/realtimeproto"
)

const (
	// readinessProbeAudio：就绪检查识别的静音有多长。只为走一遍识别器（排队拿锁、原生解码），越短越好；
	// 定值，没有单独实测（SenseVoice 识别 1 秒左右的话只要几十毫秒）
	readinessProbeAudio = 500 * time.Millisecond
	// readinessTimeout：识别这段静音最多等这么久，过了就算没就绪。要比 k8s 探针等 haro-asr -probe 的时间
	// （2 秒）短，前面排着别人的一句话时也够用。定值，没有单独实测
	readinessTimeout = time.Second
)

// recognizerReadiness 是 haro-asr 的 /readyz：在期限内真的识别完一小段静音才算就绪。识别器卡住（cgo 里挂住、
// 识别器的锁一直被占）时就绪检查跟着失败，不再只要进程能响应就回 200。
// 同一时间只识别一次：卡住时后来的探测等的是同一次识别，不会越堆越多；没在期限内识别完的那一次留着，识别完再结束。
type recognizerReadiness struct {
	transcriber Transcriber
	timeout     time.Duration
	logger      *slog.Logger

	probes  singleflight.Group
	mu      sync.Mutex
	failing bool // 上一次检查没过：状态变了才记日志，不每次探测都记
}

func newRecognizerReadiness(transcriber Transcriber, logger *slog.Logger) *recognizerReadiness {
	return &recognizerReadiness{transcriber: transcriber, timeout: readinessTimeout, logger: logger}
}

func (r *recognizerReadiness) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	err := r.check(request.Context())
	if request.Context().Err() != nil {
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "识别器没就绪: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// check 识别一小段静音，最多等 timeout。
func (r *recognizerReadiness) check(ctx context.Context) error {
	if r.transcriber == nil {
		return fmt.Errorf("没有识别引擎")
	}
	result := r.probes.DoChan("readyz", func() (any, error) {
		silence := realtimeproto.PCMAudio{Format: realtimeproto.TranscriptionFormat,
			Data: make([]byte, 2*samplesIn(readinessProbeAudio))} // 16 位：每个采样点 2 字节
		// 几次探测共用这一次识别，不随某一次探测取消；等多久由下面的期限管
		_, err := r.transcriber.Transcribe(context.WithoutCancel(ctx), silence)
		return nil, err
	})
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	var err error
	select {
	case done := <-result:
		err = done.Err
	case <-timer.C:
		err = fmt.Errorf("%v 内没识别完 %v 的静音（识别器卡住了？）", r.timeout, readinessProbeAudio)
	case <-ctx.Done():
		return ctx.Err()
	}
	r.record(err)
	return err
}

// record 记下这次检查的结果：变成没就绪时记一条警告，恢复时记一条。
func (r *recognizerReadiness) record(err error) {
	r.mu.Lock()
	changed := r.failing != (err != nil)
	r.failing = err != nil
	r.mu.Unlock()
	switch {
	case changed && err != nil:
		r.logger.Warn("就绪检查：识别器没就绪", "err", err)
	case changed:
		r.logger.Info("就绪检查：识别器恢复了")
	}
}
