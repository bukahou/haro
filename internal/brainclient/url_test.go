package brainclient

import "testing"

// 对话会话的地址按结构拼：http → ws、https → wss，已经是 ws(s) 的原样用（不会拼成 wsws://）；scheme 不分大小写；
// 保留地址里原有的路径。不是 http(s) / ws(s)、不带协议、没有主机名的地址报错。
func TestConversationURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:59130":        "ws://127.0.0.1:59130/v1/realtime",
		"http://127.0.0.1:59130/":       "ws://127.0.0.1:59130/v1/realtime",
		"HTTP://127.0.0.1:59130":        "ws://127.0.0.1:59130/v1/realtime",
		"https://haro.example.com":      "wss://haro.example.com/v1/realtime",
		"https://haro.example.com/base": "wss://haro.example.com/base/v1/realtime",
		"ws://127.0.0.1:59130":          "ws://127.0.0.1:59130/v1/realtime",
		"wss://haro.example.com/":       "wss://haro.example.com/v1/realtime",
	} {
		if got, err := conversationURL(base); err != nil || got != want {
			t.Errorf("%s → %q %v，应为 %q", base, got, err, want)
		}
	}
	for _, bad := range []string{"", "127.0.0.1:59130", "localhost:59130", "haro.example.com", "ftp://127.0.0.1", "http://", "http:///v1"} {
		if got, err := conversationURL(bad); err == nil {
			t.Errorf("%q 应报错，得到 %q", bad, got)
		}
	}
}
