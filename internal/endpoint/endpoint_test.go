package endpoint_test

import (
	"net/url"
	"testing"

	"github.com/bukahou/haro/internal/endpoint"
)

// 原有的路径、查询参数保留；写死的路径原样接上；运行时来的路径段逐段转义（/ 也转义）；查询参数合并。
func TestJoin(t *testing.T) {
	for _, c := range []struct {
		name, base, path string
		query            url.Values
		segments         []string
		want             string
	}{
		{"只接路径", "http://192.0.2.10:9880", "/tts", nil, nil, "http://192.0.2.10:9880/tts"},
		{"根地址带 / 结尾", "http://192.0.2.10:9880/", "/tts", nil, nil, "http://192.0.2.10:9880/tts"},
		{"保留原有的路径", "https://api.example.com/proxy/", "/v1", nil, nil, "https://api.example.com/proxy/v1"},
		{"保留原有的查询参数", "https://api.example.com/v1/forecast?apikey=K", "", url.Values{"latitude": {"35.6"}}, nil,
			"https://api.example.com/v1/forecast?apikey=K&latitude=35.6"},
		{"路径段转义", "https://www.example.com", "/amedas/data/point", nil, []string{"4610 6", "a/b", "日本.json"},
			"https://www.example.com/amedas/data/point/4610%206/a%2Fb/%E6%97%A5%E6%9C%AC.json"},
		{"写死的路径带 / 结尾再接段", "http://127.0.0.1:59130", "/v1/voiceprints/", nil, []string{"主人"},
			"http://127.0.0.1:59130/v1/voiceprints/%E4%B8%BB%E4%BA%BA"},
		{"什么都不接", "https://geocoding.example.com/v1/search", "", nil, nil, "https://geocoding.example.com/v1/search"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := endpoint.Join(c.base, c.path, c.query, c.segments...)
			if err != nil || got != c.want {
				t.Errorf("Join = %q, %v；应为 %q", got, err, c.want)
			}
		})
	}
}

// 地址不对、路径段是空 / . / .. 时报错，不拼出一个坏地址。
func TestJoinRejects(t *testing.T) {
	for _, c := range []struct {
		name, base string
		segments   []string
	}{
		{"没有 scheme", "192.0.2.10:9880", nil},
		{"不认识的 scheme", "ftp://192.0.2.10", nil},
		{"没有主机名", "http://", nil},
		{"WebSocket 地址不能当 HTTP 用", "ws://192.0.2.10", nil},
		{"空的路径段", "http://192.0.2.10", []string{""}},
		{"往上一级", "http://192.0.2.10", []string{".."}},
		{"当前一级", "http://192.0.2.10", []string{"."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, err := endpoint.Join(c.base, "/x", nil, c.segments...); err == nil {
				t.Errorf("应报错，得到 %q", got)
			}
		})
	}
}

// 换成 WebSocket 地址：http → ws、https → wss、已经是 ws(s) 的原样用；路径、查询参数照 Join。
func TestWebSocket(t *testing.T) {
	for _, c := range []struct {
		base  string
		query url.Values
		want  string
	}{
		{"http://192.0.2.10:59130", nil, "ws://192.0.2.10:59130/v1/realtime"},
		{"https://haro.example.com/base/", nil, "wss://haro.example.com/base/v1/realtime"},
		{"ws://192.0.2.10:59130", nil, "ws://192.0.2.10:59130/v1/realtime"},
		{"http://127.0.0.1:59126", url.Values{"intent": {"transcription"}}, "ws://127.0.0.1:59126/v1/realtime?intent=transcription"},
	} {
		got, err := endpoint.WebSocket(c.base, "/v1/realtime", c.query)
		if err != nil || got != c.want {
			t.Errorf("WebSocket(%q) = %q, %v；应为 %q", c.base, got, err, c.want)
		}
	}
	if got, err := endpoint.WebSocket("ftp://192.0.2.10", "/v1/realtime", nil); err == nil {
		t.Errorf("不认识的 scheme 应报错，得到 %q", got)
	}
}
