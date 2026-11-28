package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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
	go func() { _, err := home.Home(impatient, ""); first <- err }()
	time.Sleep(50 * time.Millisecond) // 第一个在等定位服务
	second := make(chan Place, 1)
	go func() { place, _ := home.Home(context.Background(), ""); second <- place }()

	started := time.Now()
	if _, _, ok := home.Known(""); ok || time.Since(started) > 20*time.Millisecond {
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
	if place, byIP, ok := home.Known(""); !ok || !byIP || place.Name != "Fuchu" {
		t.Errorf("定位完应记下：%+v %v %v", place, byIP, ok)
	}
}

// fakeClock 是测试里拨得动的钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// locatorServer 是假的定位服务：down 时回 404（不重试）；gate 设了就等它关上再回；记下请求次数。
type locatorServer struct {
	*httptest.Server
	requests atomic.Int32
	down     atomic.Bool
	city     atomic.Value // string
	gate     atomic.Pointer[chan struct{}]
}

func newLocatorServer(t *testing.T) *locatorServer {
	t.Helper()
	server := &locatorServer{}
	server.city.Store("Sapporo")
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.requests.Add(1)
		if gate := server.gate.Load(); gate != nil {
			<-*gate
		}
		if server.down.Load() {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"city":"` + server.city.Load().(string) + `","region":"Hokkaido","country":"JP","loc":"43.06,141.35"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// waitForRefresh 等后台那次请求结束：同一个键上正在进行的请求，Do 会等它结束；没有就什么都不做。
func waitForRefresh[T any](lookup *cachedLookup[T]) {
	lookup.requests.Do(lookupKey, func() (any, error) { return nil, nil })
}

// 还没定位成功过、定位服务又挂着：报错之后一分钟内不再去问它（直接报错，不让每次查天气都等它超时）；过了再试。
func TestIPHomeCoolsDownAfterFailure(t *testing.T) {
	ctx := context.Background()
	server := newLocatorServer(t)
	server.down.Store(true)
	home := NewIPHome(server.URL, "", server.Client())
	clock := newFakeClock()
	home.location.now = clock.Now

	if _, err := home.Home(ctx, ""); err == nil || server.requests.Load() != 1 {
		t.Fatalf("定位服务挂着：应报错：%v（问了 %d 次）", err, server.requests.Load())
	}
	if _, err := home.Home(ctx, ""); err == nil || server.requests.Load() != 1 {
		t.Errorf("冷却期内不再问定位服务，直接报错：%v（问了 %d 次）", err, server.requests.Load())
	}
	clock.Advance(lookupRetryAfter)
	server.down.Store(false)
	if place, err := home.Home(ctx, ""); err != nil || place.Name != "Sapporo" || server.requests.Load() != 2 {
		t.Errorf("过了冷却期再试：%+v %v（问了 %d 次）", place, err, server.requests.Load())
	}
}

// 位置过期了：马上用上次的，同时在后台重新定位（查天气不等它）；重新定位好了换成新的。
func TestIPHomeUsesStaleLocationWhileRefreshing(t *testing.T) {
	ctx := context.Background()
	server := newLocatorServer(t)
	home := NewIPHome(server.URL, "", server.Client())
	clock := newFakeClock()
	home.location.now = clock.Now
	if _, err := home.Home(ctx, ""); err != nil {
		t.Fatal(err)
	}

	clock.Advance(ipHomeTTL)
	gate := make(chan struct{})
	var opened sync.Once
	openGate := func() { opened.Do(func() { close(gate) }) }
	t.Cleanup(openGate)      // 测试中途失败时也放行（不然关假服务时会一直等这个请求）
	server.gate.Store(&gate) // 重新定位卡在定位服务那里
	server.city.Store("Otaru")
	located := make(chan Place, 1)
	go func() { place, _ := home.Home(ctx, ""); located <- place }()
	select {
	case place := <-located:
		if place.Name != "Sapporo" {
			t.Errorf("过期后应先用上次的位置：%+v", place)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("过期后不该等重新定位")
	}
	openGate()
	waitForRefresh(home.location)
	if place, _, ok := home.Known(""); !ok || place.Name != "Otaru" || server.requests.Load() != 2 {
		t.Errorf("后台重新定位好了应换成新的：%+v %v（问了 %d 次）", place, ok, server.requests.Load())
	}
}

// 位置过期、后台重新定位又失败：照样用上次的；一分钟内不再去问；过了再在后台试。
func TestIPHomeStaleRefreshFailureCoolsDown(t *testing.T) {
	ctx := context.Background()
	server := newLocatorServer(t)
	home := NewIPHome(server.URL, "", server.Client())
	clock := newFakeClock()
	home.location.now = clock.Now
	if _, err := home.Home(ctx, ""); err != nil {
		t.Fatal(err)
	}

	clock.Advance(ipHomeTTL)
	server.down.Store(true)
	if place, err := home.Home(ctx, ""); err != nil || place.Name != "Sapporo" {
		t.Fatalf("过期后先用上次的：%+v %v", place, err)
	}
	waitForRefresh(home.location)
	if place, err := home.Home(ctx, ""); err != nil || place.Name != "Sapporo" || server.requests.Load() != 2 {
		t.Errorf("后台重新定位失败：照样用上次的，冷却期内不再问：%+v %v（问了 %d 次）", place, err, server.requests.Load())
	}
	waitForRefresh(home.location)
	if n := server.requests.Load(); n != 2 {
		t.Errorf("冷却期内不该在后台再问：问了 %d 次", n)
	}

	clock.Advance(lookupRetryAfter)
	server.down.Store(false)
	server.city.Store("Otaru")
	if _, err := home.Home(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitForRefresh(home.location)
	if place, _, _ := home.Known(""); place.Name != "Otaru" || server.requests.Load() != 3 {
		t.Errorf("过了冷却期在后台再试：%+v（问了 %d 次）", place, server.requests.Load())
	}
}
