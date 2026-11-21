package weather

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// lookupRetryAfter 是家的位置、气象厅的地区表拿不到之后，多久之内不再去拿：服务挂着时，不让每次查天气都先等它
// 超时（一次要等好几秒）。定值：比主人连着问两句的间隔长；服务恢复后，最多晚这么久用上。
const lookupRetryAfter = time.Minute

// lookupKey 是 cachedLookup 里共享请求的键（每个 cachedLookup 只缓存一个值）。
const lookupKey = "value"

// cachedLookup 缓存一个要发网络请求才拿得到、又很少变的值（家的位置、气象厅的地区表）：
//   - 有效期内直接用（ttl 为 0 = 拿到了就一直用）；
//   - 过期了：马上用旧值，同时在后台刷新，查天气不等它；
//   - 还没有值：当场去拿，同时要的只发一次请求（请求不在锁里发）；
//   - 拿不到时记下时间，retryAfter 之内不再去拿：有旧值用旧值，没有就直接报上次的错。
//
// 请求不跟着某一个调用方的 context（见 sharedRequest），受每次请求的时限约束。
type cachedLookup[T any] struct {
	fetch      func(context.Context) (T, error)
	ttl        time.Duration
	retryAfter time.Duration
	now        func() time.Time

	requests  singleflight.Group
	mu        sync.Mutex
	value     T
	fetchedAt time.Time // 零值 = 还没拿到过
	failedAt  time.Time // 最近一次没拿到；拿到后清掉
	failure   error
}

func newCachedLookup[T any](fetch func(context.Context) (T, error), ttl time.Duration) *cachedLookup[T] {
	return &cachedLookup[T]{fetch: fetch, ttl: ttl, retryAfter: lookupRetryAfter, now: time.Now}
}

// Get 返回值；只有还没拿到过、又拿不到时才报错。
func (c *cachedLookup[T]) Get(ctx context.Context) (T, error) {
	c.mu.Lock()
	value, fetchedAt, failedAt, failure := c.value, c.fetchedAt, c.failedAt, c.failure
	c.mu.Unlock()
	now := c.now()
	coolingDown := !failedAt.IsZero() && now.Sub(failedAt) < c.retryAfter
	switch {
	case !fetchedAt.IsZero() && (c.ttl == 0 || now.Sub(fetchedAt) < c.ttl):
		return value, nil
	case !fetchedAt.IsZero(): // 过期了：先用旧值；后台刷新（刚失败过就先不刷）
		if !coolingDown {
			c.requests.DoChan(lookupKey, func() (any, error) { return c.fetchAndRemember(context.WithoutCancel(ctx)) })
		}
		return value, nil
	case coolingDown:
		var zero T
		return zero, fmt.Errorf("刚才就没拿到，过一会儿再试: %w", failure)
	}
	return sharedRequest(ctx, &c.requests, lookupKey, c.fetchAndRemember)
}

// Known 返回已经拿到的值（不发请求，过期的也算）；还没拿到过时 ok = false。
func (c *cachedLookup[T]) Known() (value T, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, !c.fetchedAt.IsZero()
}

// fetchAndRemember 发一次请求，记下结果：拿到了换掉旧值，没拿到记下时间和原因（旧值留着）。
func (c *cachedLookup[T]) fetchAndRemember(ctx context.Context) (T, error) {
	value, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failedAt, c.failure = c.now(), err
		return value, err
	}
	c.value, c.fetchedAt, c.failedAt, c.failure = value, c.now(), time.Time{}, nil
	return value, nil
}
