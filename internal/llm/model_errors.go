package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

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
// 认证失败（401 / 403）、请求超时（408）、限流（429）、服务端出错或过载（5xx、529）。
// 流已经开始（HTTP 回了 200）之后才来的错误事件同样按这几类认。请求本身有问题（400 等）不算。
func unavailable(err error) bool {
	var anthropicErr *anthropic.Error
	var openaiErr *openai.Error
	var streamErr *ssestream.StreamError
	var netErr net.Error
	switch {
	case errors.Is(err, errNoFirstToken), errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.As(err, &anthropicErr) && anthropicErr.StatusCode == http.StatusOK:
		// 流开始之后来的 error 事件：SDK 给的状态码是流的 200，按错误体里的错误类型认
		return slices.Contains(unavailableClaudeErrorTypes, anthropicErr.Type())
	case errors.As(err, &anthropicErr):
		return unavailableStatus(anthropicErr.StatusCode)
	case errors.As(err, &openaiErr):
		return unavailableStatus(openaiErr.StatusCode)
	case errors.As(err, &streamErr):
		// OpenAI 兼容接口流开始之后来的错误事件：错误体带数字状态码的按状态码认；不带的（各家写法不一）是服务端接下请求后
		// 生成途中出错，和流读到一半断了同一类
		if code, ok := streamErrorStatus(streamErr); ok {
			return unavailableStatus(code)
		}
		return true
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

// unavailableClaudeErrorTypes 是算连不上的 Claude 错误类型（错误体的 error.type），和 unavailableStatus 的状态码对应：
// 认证失败 401、没有权限 403、限流 429、服务端出错 500、超时、过载 529。
var unavailableClaudeErrorTypes = []anthropic.ErrorType{anthropic.ErrorTypeAuthenticationError, anthropic.ErrorTypePermissionError,
	anthropic.ErrorTypeRateLimitError, anthropic.ErrorTypeAPIError, anthropic.ErrorTypeTimeoutError, anthropic.ErrorTypeOverloadedError}

// streamErrorStatus 取 OpenAI 兼容接口流里错误事件的状态码：有的服务把 HTTP 状态码写在错误体的 error.code 里（数字）。
// 不是数字（字符串、null、没有）时 ok = false。
func streamErrorStatus(streamErr *ssestream.StreamError) (code int, ok bool) {
	var body struct {
		Error struct {
			Code *int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(streamErr.Event.Data, &body) != nil || body.Error.Code == nil {
		return 0, false
	}
	return *body.Error.Code, true
}
