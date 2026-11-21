package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// clearOTelEnv 去掉跑测试那台机器上的 OTEL_* 变量（测试结束后恢复），只看测试自己设的。
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envEndpoint, envTracesEndpoint, envMetricsEndpoint, envProtocol, envTracesProtocol,
		envMetricsProtocol, "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// 往哪发：分信号的标准变量、通用的标准变量、HARO_OTLP_ENDPOINT，先设了的为准；都没有就不导出。传输方式只认 http/protobuf。
func TestExportTarget(t *testing.T) {
	for _, c := range []struct {
		name     string
		env      map[string]string
		fallback string
		want     target
		fails    bool
	}{
		{name: "都没设：不导出"},
		{name: "只有 HARO_OTLP_ENDPOINT：拼出 /v1/traces", fallback: "http://collector:4318/",
			want: target{enabled: true, endpointURL: "http://collector:4318/v1/traces"}},
		{name: "标准变量设了：以它为准（交给 SDK 读）", env: map[string]string{envEndpoint: "http://otel:4318"},
			fallback: "http://collector:4318", want: target{enabled: true}},
		{name: "分信号的标准变量", env: map[string]string{envTracesEndpoint: "http://otel:4318/v1/traces"},
			fallback: "http://collector:4318", want: target{enabled: true}},
		{name: "只设了指标的：追踪照样用 HARO_OTLP_ENDPOINT", env: map[string]string{envMetricsEndpoint: "http://otel:4318/v1/metrics"},
			fallback: "http://collector:4318", want: target{enabled: true, endpointURL: "http://collector:4318/v1/traces"}},
		{name: "写明 http/protobuf", env: map[string]string{envProtocol: httpProtobuf}, fallback: "http://collector:4318",
			want: target{enabled: true, endpointURL: "http://collector:4318/v1/traces"}},
		{name: "gRPC 不支持：报错", env: map[string]string{envProtocol: "grpc"}, fallback: "http://collector:4318", fails: true},
		{name: "分信号写 gRPC 也报错", env: map[string]string{envTracesProtocol: "grpc"}, fails: true},
	} {
		getenv := func(name string) string { return c.env[name] }
		got, err := exportTarget(getenv, envTracesEndpoint, envTracesProtocol, "traces", c.fallback)
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("%s：%+v %v", c.name, got, err)
		}
	}
}

// 每个程序报自己的 service.name 和版本号；标准的 OTEL_SERVICE_NAME、OTEL_RESOURCE_ATTRIBUTES 覆盖、合并进来，写错时报错。
func TestResourceNamesEachProgram(t *testing.T) {
	clearOTelEnv(t)
	value := func(serviceResource interface{ Set() *attribute.Set }, key attribute.Key) string {
		got, _ := serviceResource.Set().Value(key)
		return got.AsString()
	}
	terminal, err := newResource(context.Background(), ServiceTerminal)
	if err != nil || value(terminal, semconv.ServiceNameKey) != ServiceTerminal {
		t.Fatalf("终端报 %s：%v %v", ServiceTerminal, terminal, err)
	}
	if server, _ := newResource(context.Background(), ServiceServer); value(server, semconv.ServiceNameKey) != ServiceServer {
		t.Errorf("服务端报 %s：%v", ServiceServer, server)
	}
	t.Setenv("OTEL_SERVICE_NAME", "haro-server-dev")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=dev")
	overridden, err := newResource(context.Background(), ServiceServer)
	if err != nil || value(overridden, semconv.ServiceNameKey) != "haro-server-dev" || value(overridden, "deployment.environment") != "dev" {
		t.Errorf("标准变量应覆盖、合并进来：%v %v", overridden, err)
	}
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "没有等号")
	if _, err := newResource(context.Background(), ServiceServer); err == nil {
		t.Error("OTEL_RESOURCE_ATTRIBUTES 写错时应报错")
	}
}

// collector 是假的 OTLP/HTTP 采集器：记下收到的请求路径。
type collector struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
}

func newCollector(t *testing.T) *collector {
	c := &collector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.paths)
}

// emitAndShutdown 记一个 span、一个计数，再关掉（关的时候把还没发的发出去）。
func emitAndShutdown(t *testing.T, shutdown func() error) {
	t.Helper()
	_, span := otel.Tracer("test").Start(context.Background(), "测试")
	span.End()
	counter := MustCounter(otel.Meter("test"), "haro.test.count", "测试")
	counter.Add(context.Background(), 1)
	if err := shutdown(); err != nil {
		t.Errorf("关掉遥测：%v", err)
	}
}

// 只配了 HARO_OTLP_ENDPOINT：追踪、指标发到它下面的 /v1/traces、/v1/metrics；标准变量设了时改发标准变量指的地方。
func TestSetupExportsToConfiguredEndpoint(t *testing.T) {
	clearOTelEnv(t)
	haro := newCollector(t)
	shutdown, err := Setup(context.Background(), ServiceServer, haro.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	emitAndShutdown(t, shutdown)
	if got := haro.received(); !slices.Contains(got, "/v1/traces") || !slices.Contains(got, "/v1/metrics") {
		t.Errorf("HARO_OTLP_ENDPOINT 应收到追踪和指标：%v", got)
	}

	standard, unused := newCollector(t), newCollector(t)
	t.Setenv(envEndpoint, standard.server.URL)
	shutdown, err = Setup(context.Background(), ServiceServer, unused.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	emitAndShutdown(t, shutdown)
	if got := standard.received(); !slices.Contains(got, "/v1/traces") || len(unused.received()) != 0 {
		t.Errorf("标准变量设了时应发到它指的地方：标准 %v，HARO_OTLP_ENDPOINT %v", got, unused.received())
	}
}

// 退出时等遥测发出去有上限：采集器连上了却不回应时，关掉遥测在期限内返回，不拖住退出。
func TestShutdownIsBounded(t *testing.T) {
	clearOTelEnv(t)
	hang := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(hang)
		stuck.Close()
	})
	shutdown, err := setup(context.Background(), ServiceTerminal, stuck.URL, os.Getenv, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "测试")
	span.End()
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- shutdown() }()
	select {
	case err := <-result:
		if err == nil {
			t.Error("采集器不回应时应交出没发出去的错误")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("关掉遥测应在期限（200 ms）附近返回，用了 %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("采集器不回应时关掉遥测一直不返回")
	}
}
