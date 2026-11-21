package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/openai/openai-go/v3"

	"github.com/bukahou/haro/internal/brain"
)

// markUnavailable 把「连不上模型服务」的错误标成 brain.ErrModelUnavailable（errors.Is 判断），别的原样返回。
// 调用方自己取消（主人打断、调查到了总时限）不算：那不是服务的问题。
func markUnavailable(callerCtx context.Context, err error) error {
	if err == nil || callerCtx.Err() != nil || !unavailable(err) {
		return err
	}
	return fmt.Errorf("%w: %w", brain.ErrModelUnavailable, err)
}

// unavailable 判断是不是连不上：网络错误（连不上、DNS、连接被重置、流读到一半断了）、等不到开始回答、整次请求超时、
// 认证失败（401 / 403）、请求超时（408）、限流（429）、服务端出错或过载（5xx、529）。请求本身有问题（400 等）不算。
func unavailable(err error) bool {
	var anthropicErr *anthropic.Error
	var openaiErr *openai.Error
	var netErr net.Error
	switch {
	case errors.Is(err, errNoFirstToken), errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.As(err, &anthropicErr):
		return unavailableStatus(anthropicErr.StatusCode)
	case errors.As(err, &openaiErr):
		return unavailableStatus(openaiErr.StatusCode)
	case errors.As(err, &netErr):
		return true
	}
	return false
}

func unavailableStatus(code int) bool {
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusRequestTimeout,
		code == http.StatusTooManyRequests, code >= http.StatusInternalServerError:
		return true
	}
	return false
}
