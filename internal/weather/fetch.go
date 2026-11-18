package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

// 每次请求最多等 3.5 秒；连上了却卡住等不到回复时再试一次（两次合计 7 秒，留在服务端 10 秒的工具时限内）。
// 建立连接（TCP 连接、TLS 握手）各最多等 1.5 秒，连接建立不起来不重试：对方不接受连接、或连上了却不握手时，
// 再试也是白等，早点交给备用来源。
const (
	defaultAttemptTimeout = 3500 * time.Millisecond
	maxAttempts           = 2
	connectTimeout        = 1500 * time.Millisecond
)

// NewHTTPClient 是查天气用的 HTTP 客户端：TCP 连接、TLS 握手各最多等 1.5 秒；其余的时限由每次请求的 context 管。
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = connectTimeout
	return &http.Client{Transport: transport}
}

// fetcher 发 GET 请求：每次最多等 attemptTimeout，连上后卡住时再试一次；连接建立不起来、对方报 4xx、回复看不懂都不重试。
type fetcher struct {
	http           *http.Client
	attemptTimeout time.Duration
	service        string // 报错时说是哪个服务
}

func (f fetcher) getJSON(ctx context.Context, address string, target any) error {
	return f.get(ctx, address, func(body io.Reader) error { return json.NewDecoder(body).Decode(target) })
}

func (f fetcher) getText(ctx context.Context, address string) (string, error) {
	var text string
	err := f.get(ctx, address, func(body io.Reader) error {
		raw, err := io.ReadAll(io.LimitReader(body, 1<<16))
		text = strings.TrimSpace(string(raw))
		return err
	})
	return text, err
}

func (f fetcher) get(ctx context.Context, address string, decode func(io.Reader) error) error {
	timeout := f.attemptTimeout
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var retry bool
		retry, err = f.getOnce(ctx, address, timeout, decode)
		if err == nil || !retry || ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf("%w（试了 %d 次）", err, maxAttempts)
}

func (f fetcher) getOnce(ctx context.Context, address string, timeout time.Duration, decode func(io.Reader) error) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var connected atomic.Bool // 连接（含 TLS 握手）建立起来了
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, err
	}
	response, err := f.http.Do(request)
	if err != nil {
		return connected.Load(), fmt.Errorf("%s连不上: %w", f.name(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode >= 500, fmt.Errorf("%s报错: HTTP %d", f.name(), response.StatusCode)
	}
	if err := decode(response.Body); err != nil {
		return ctx.Err() != nil, fmt.Errorf("%s的回复看不懂: %w", f.name(), err)
	}
	return false, nil
}

func (f fetcher) name() string {
	if f.service == "" {
		return "天气服务"
	}
	return f.service
}
