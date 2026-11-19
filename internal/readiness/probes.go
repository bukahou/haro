package readiness

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// HTTPStatusOK 检查一个就绪地址：返回 200 才算正常（例如 haro-asr 的 /readyz）。
func HTTPStatusOK(client *http.Client, address string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		status, err := get(ctx, client, address)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s 返回 %d", address, status)
		}
		return nil
	}
}

// HTTPReachable 检查一个服务连不连得上：收到任何 HTTP 响应都算（用于没有健康检查地址的外部服务，例如语音合成）。
func HTTPReachable(client *http.Client, address string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := get(ctx, client, address)
		return err
	}
}

func get(ctx context.Context, client *http.Client, address string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode, nil
}
