package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// 第一轮检查做完之前不算就绪；各项都正常才就绪；有一项出错就是 503，内容里写明哪一项、为什么。
func TestMonitorReport(t *testing.T) {
	failing := errors.New("连不上")
	var asrErr error
	monitor := NewMonitor([]Check{
		{Name: "asr", Probe: func(context.Context) error { return asrErr }},
		{Name: "tts", Probe: func(context.Context) error { return nil }},
	}, time.Hour, time.Second, discard)

	if report := monitor.Report(); report.Ready || report.Checks[0].Error != errNotCheckedYet.Error() {
		t.Fatalf("还没检查过时不算就绪：%+v", report)
	}
	monitor.checkAll(context.Background())
	if report := monitor.Report(); !report.Ready {
		t.Fatalf("各项正常时应就绪：%+v", report)
	}

	asrErr = failing
	monitor.checkAll(context.Background())
	recorder := httptest.NewRecorder()
	monitor.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var report Report
	if err := json.NewDecoder(recorder.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || report.Ready || report.Checks[0].OK || report.Checks[0].Error != failing.Error() ||
		!report.Checks[1].OK {
		t.Errorf("有一项出错时应是 503 并写明哪一项：%d %+v", recorder.Code, report)
	}
}

// 每项检查受 timeout 约束：卡住的检查不会拖住整轮。
func TestMonitorProbeTimeout(t *testing.T) {
	monitor := NewMonitor([]Check{{Name: "slow", Probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}}, time.Hour, 20*time.Millisecond, discard)
	started := time.Now()
	monitor.checkAll(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("卡住的检查应在期限内结束：%v", elapsed)
	}
	if report := monitor.Report(); report.Ready || report.Checks[0].CheckedAt.IsZero() {
		t.Errorf("超时算出错：%+v", report)
	}
}

func TestHTTPProbes(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer server.Close()
	ctx := context.Background()
	if err := HTTPStatusOK(server.Client(), server.URL)(ctx); err != nil {
		t.Errorf("200 应算正常：%v", err)
	}
	status = http.StatusNotFound
	if err := HTTPStatusOK(server.Client(), server.URL)(ctx); err == nil {
		t.Error("不是 200 应算出错")
	}
	if err := HTTPReachable(server.Client(), server.URL)(ctx); err != nil {
		t.Errorf("收到任何响应都算连得上：%v", err)
	}
	server.Close()
	if err := HTTPReachable(http.DefaultClient, server.URL)(ctx); err == nil {
		t.Error("连不上应算出错")
	}
}

// levelCounter 记各级别的日志条数。
type levelCounter struct {
	counts map[slog.Level]int
}

func (c *levelCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *levelCounter) Handle(_ context.Context, record slog.Record) error {
	c.counts[record.Level]++
	return nil
}
func (c *levelCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *levelCounter) WithGroup(string) slog.Handler      { return c }

// 启动后第一轮就出错（一起启动的依赖晚一两秒才开始监听）：只记 INFO，下一轮好了也不算出过错；
// 第二轮还出错才记 WARN。就绪的结果照实（第一轮就是 503）。
func TestStartupFailureIsNotAWarning(t *testing.T) {
	counter := &levelCounter{counts: map[slog.Level]int{}}
	var asrErr = errors.New("connection refused")
	monitor := NewMonitor([]Check{{Name: "asr", Probe: func(context.Context) error { return asrErr }}},
		time.Hour, time.Second, slog.New(counter))
	monitor.checkAll(context.Background())
	if report := monitor.Report(); report.Ready {
		t.Fatal("第一轮出错就不算就绪（照实）")
	}
	if counter.counts[slog.LevelWarn] != 0 || counter.counts[slog.LevelInfo] != 1 {
		t.Fatalf("启动后第一轮出错只记 INFO：%v", counter.counts)
	}
	asrErr = nil
	monitor.checkAll(context.Background())
	if counter.counts[slog.LevelWarn] != 0 || !monitor.Report().Ready {
		t.Fatalf("第二轮好了：不记 WARN、就绪：%v", counter.counts)
	}

	// 阳性对照：启动后第一、二轮都出错，第二轮记 WARN
	counter.counts = map[slog.Level]int{}
	asrErr = errors.New("connection refused")
	stuck := NewMonitor([]Check{{Name: "asr", Probe: func(context.Context) error { return asrErr }}},
		time.Hour, time.Second, slog.New(counter))
	stuck.checkAll(context.Background())
	stuck.checkAll(context.Background())
	if counter.counts[slog.LevelWarn] != 1 {
		t.Errorf("第二轮还出错应记 WARN：%v", counter.counts)
	}
}
