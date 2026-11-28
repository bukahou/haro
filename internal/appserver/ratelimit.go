package appserver

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/bukahou/haro/internal/appapi"
)

// 限流：按来源 IP，令牌桶（golang.org/x/time/rate）。登录、刷新这类公网上能反复试探的接口用严的一档，其余用宽的一档。
const (
	strictPerMinute = 10  // 登录、刷新、登出、配对：每个来源每分钟
	normalPerMinute = 240 // 其余接口：每个来源每分钟
	limiterIdle     = 10 * time.Minute
	limiterMaxKeys  = 4096 // 记着的来源最多这么多（超过时先清掉闲着的）
)

// strictPaths 是用严的一档的路径（前缀）。
var strictPaths = []string{"/v1/auth/", "/v1/terminals/pair"}

type limitEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
}

func newRateLimiter(ctx context.Context) *rateLimiter {
	limiter := &rateLimiter{entries: map[string]*limitEntry{}}
	go limiter.sweep(ctx)
	return limiter
}

// allow 判断这个来源（加上是哪一档）现在能不能再请求一次。
func (l *rateLimiter) allow(key string, perMinute int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.entries) >= limiterMaxKeys {
		l.dropIdle(now)
	}
	entry, ok := l.entries[key]
	if !ok {
		// 一分钟的额度平均放出；允许一下子连着来半分钟的量（登录完紧接着刷新这类）
		entry = &limitEntry{limiter: rate.NewLimiter(rate.Limit(float64(perMinute)/60), max(perMinute/2, 1))}
		l.entries[key] = entry
	}
	entry.seen = now
	return entry.limiter.Allow()
}

func (l *rateLimiter) dropIdle(now time.Time) {
	for key, entry := range l.entries {
		if now.Sub(entry.seen) > limiterIdle {
			delete(l.entries, key)
		}
	}
}

func (l *rateLimiter) sweep(ctx context.Context) {
	ticker := time.NewTicker(limiterIdle)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			l.mu.Lock()
			l.dropIdle(now)
			l.mu.Unlock()
		}
	}
}

// wrap 给接口套上限流：超过时回 429 + Retry-After。
func (l *rateLimiter) wrap(next http.Handler, server *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perMinute, tier := normalPerMinute, "normal"
		for _, prefix := range strictPaths {
			if strings.HasPrefix(r.URL.Path, prefix) {
				perMinute, tier = strictPerMinute, "strict"
			}
		}
		if !l.allow(tier+"|"+clientIP(r), perMinute) {
			w.Header().Set("Retry-After", strconv.Itoa(max(60/perMinute, 1)))
			server.writeProblem(w, r, http.StatusTooManyRequests, appapi.ErrorCodeRateLimited, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP 是请求的来源 IP：经 Cloudflare 进来的取它给的 CF-Connecting-IP（隧道之后看到的都是隧道的地址），
// 否则取连接的地址。
func clientIP(r *http.Request) string {
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); ip != nil {
		return ip.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
