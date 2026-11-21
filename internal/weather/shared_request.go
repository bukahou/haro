package weather

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// sharedRequest 让同时要同一样东西的几个调用只发一次请求，而且请求不在任何锁里发。
// 请求不跟着某一个调用方的 context（先来的那个被打断，后面等着的照样拿到结果；请求本身受每次请求的时限约束），
// 每个调用方按自己的 context 等，等不及就先走。
func sharedRequest[T any](ctx context.Context, group *singleflight.Group, key string, fetch func(context.Context) (T, error)) (T, error) {
	results := group.DoChan(key, func() (any, error) { return fetch(context.WithoutCancel(ctx)) })
	var zero T
	select {
	case result := <-results:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(T), nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}
