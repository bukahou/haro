package weather

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubForecaster struct {
	forecast Forecast
	err      error
	calls    int
}

func (s *stubForecaster) ForecastFor(context.Context, Place, int) (Forecast, error) {
	s.calls++
	return s.forecast, s.err
}

func TestBackupForecaster(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	down := errors.New("天气服务连不上")
	backupResult := Forecast{Source: "日本气象厅", Note: "按地区的预报。"}
	cases := []struct {
		name               string
		primary, backup    *stubForecaster
		canceled           bool
		wantSource         string
		wantErr            error
		wantBackupCalls    int
		wantNote, wantText string
	}{
		{name: "主力查到了：不问备用", primary: &stubForecaster{forecast: Forecast{Source: "Open-Meteo"}}, backup: &stubForecaster{},
			wantSource: "Open-Meteo"},
		{name: "主力连不上：用备用，并说明", primary: &stubForecaster{err: down}, backup: &stubForecaster{forecast: backupResult},
			wantSource: "日本气象厅", wantBackupCalls: 1, wantNote: "Open-Meteo这次没查到，用的是备用的日本气象厅。按地区的预报。"},
		{name: "备用不覆盖这个地点：报主力的错", primary: &stubForecaster{err: down},
			backup: &stubForecaster{err: fmt.Errorf("%w：巴黎", ErrNotCovered)}, wantErr: down, wantBackupCalls: 1},
		{name: "两边都不行：两个原因都说", primary: &stubForecaster{err: down}, backup: &stubForecaster{err: errors.New("气象厅报错: HTTP 503")},
			wantErr: down, wantBackupCalls: 1, wantText: "HTTP 503"},
		{name: "主人打断了：不再查备用", primary: &stubForecaster{err: down}, backup: &stubForecaster{forecast: backupResult},
			canceled: true, wantErr: down},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		if c.canceled {
			cancel()
		}
		forecast, err := WithBackup(c.primary, "Open-Meteo", c.backup, "日本气象厅", logger).ForecastFor(ctx, Place{Name: "家"}, 3)
		cancel()
		switch {
		case c.wantErr == nil && err != nil:
			t.Errorf("%s：不该出错：%v", c.name, err)
		case c.wantErr != nil && !errors.Is(err, c.wantErr):
			t.Errorf("%s：应报主力的错，实际 %v", c.name, err)
		case c.wantText != "" && !strings.Contains(fmt.Sprint(err), c.wantText):
			t.Errorf("%s：错误里应有「%s」：%v", c.name, c.wantText, err)
		case c.wantErr == nil && (forecast.Source != c.wantSource || forecast.Note != c.wantNote):
			t.Errorf("%s：得到 %q / %q", c.name, forecast.Source, forecast.Note)
		case c.backup.calls != c.wantBackupCalls:
			t.Errorf("%s：备用被问了 %d 次，应为 %d", c.name, c.backup.calls, c.wantBackupCalls)
		}
	}
}

// 连接都建立不起来时不重试（再试也是白等，早点交给备用）；连上后卡住等不到回复时才重试（TestRetriesOnceOnTimeout）。
func TestDoesNotRetryWhenItCannotConnect(t *testing.T) {
	var dials atomic.Int32
	client := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}}}
	fetch := fetcher{http: client, attemptTimeout: time.Second, service: "测试服务"}
	err := fetch.getJSON(context.Background(), "http://weather.example/forecast", &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "测试服务连不上") || dials.Load() != 1 {
		t.Errorf("应只连 1 次就报连不上：%v，连了 %d 次", err, dials.Load())
	}
}

// 连上了却不做 TLS 握手（真实遇到过的故障：TCP 能连上，握手一直等不到回复）：也算连接没建立起来，不重试。
func TestDoesNotRetryWhenTLSHandshakeHangs(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer conn.Close() // 接受连接，但一个字节也不回
		}
	}()
	fetch := fetcher{http: &http.Client{}, attemptTimeout: 200 * time.Millisecond, service: "测试服务"}
	started := time.Now()
	err = fetch.getJSON(context.Background(), "https://"+listener.Addr().String()+"/forecast", &struct{}{})
	if err == nil || accepted.Load() != 1 || time.Since(started) > time.Second {
		t.Errorf("握手卡住应只连 1 次、约 200 ms 后放弃：%v，连了 %d 次，用了 %v", err, accepted.Load(), time.Since(started))
	}
}
