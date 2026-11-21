package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 按 IP 定位要发网络请求：Known（每轮都调用）不等它；同时要的只发一次请求；先来的被打断，后面等着的照样拿到结果。
func TestIPHomeLocatesOutsideTheLock(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"city":"Fuchu","region":"Tokyo","country":"JP","loc":"35.66,139.48"}`))
	}))
	defer server.Close()
	home := NewIPHome(server.URL, "", server.Client())

	impatient, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := home.Home(impatient); first <- err }()
	time.Sleep(50 * time.Millisecond) // 第一个在等定位服务
	second := make(chan Place, 1)
	go func() { place, _ := home.Home(context.Background()); second <- place }()

	started := time.Now()
	if _, _, ok := home.Known(); ok || time.Since(started) > 20*time.Millisecond {
		t.Errorf("Known 不该等定位：ok=%v 用了 %v", ok, time.Since(started))
	}
	cancel() // 先来的被打断
	if err := <-first; err == nil {
		t.Error("被打断的应返回错误")
	}
	close(release)
	if place := <-second; place.Name != "Fuchu" {
		t.Errorf("后面等着的应拿到结果：%+v", place)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("同时要的应只发一次请求：%d 次", n)
	}
	if place, byIP, ok := home.Known(); !ok || !byIP || place.Name != "Fuchu" {
		t.Errorf("定位完应记下：%+v %v %v", place, byIP, ok)
	}
}
