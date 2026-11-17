package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 第一次请求卡住（超时）、第二次正常：自动重试成功；服务报 4xx 不重试。
func TestRetriesOnceOnTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Query().Get("name") == "bad" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if n == 1 {
			select { // 第一次卡住
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"name":"测试市","latitude":1,"longitude":2}]}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, server.URL, server.Client())
	client.attemptTimeout = 100 * time.Millisecond

	place, err := client.FindPlace(context.Background(), "测试", "ja")
	if err != nil || place.Name != "测试市" || calls.Load() != 2 {
		t.Fatalf("超时后应重试一次并成功：%+v %v 请求了 %d 次", place, err, calls.Load())
	}
	calls.Store(10)
	if _, err := client.FindPlace(context.Background(), "bad", "ja"); err == nil || calls.Load() != 11 {
		t.Errorf("服务报 4xx 不重试：%v 请求了 %d 次", err, calls.Load()-10)
	}
}
