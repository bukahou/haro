// Package readiness 是服务的就绪检查：后台定期检查各项依赖（识别、合成、模型凭证、账本……），缓存结果；
// /readyz 只读缓存，不在每次探测时真的去调外部服务。
//
// 就绪只表示「现在能不能正常工作」，让问题看得见（k8s 事件、监控）；它不触发重启（那是存活检查 /livez 的事），
// 部署时也不靠它切走流量（只有一个 Pod）。
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check 是就绪检查的一项。
type Check struct {
	Name  string                          // 例如 asr、tts、llm、ledger
	Probe func(ctx context.Context) error // 返回 nil = 正常
}

// Result 是一项检查最近一次的结果。
type Result struct {
	Name      string    `json:"name"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"` // 零值 = 还没检查过
}

// Report 是 /readyz 返回的内容。
type Report struct {
	Ready  bool     `json:"ready"`
	Checks []Result `json:"checks"`
}

// errNotCheckedYet：启动后第一轮检查还没做完。
var errNotCheckedYet = errors.New("还没检查过")

// Monitor 定期跑各项检查并缓存结果。
type Monitor struct {
	checks   []Check
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	results []Result
}

// NewMonitor 创建就绪检查：每 interval 检查一轮，每项最多等 timeout。调用 Run 之后才开始检查。
func NewMonitor(checks []Check, interval, timeout time.Duration, logger *slog.Logger) *Monitor {
	results := make([]Result, len(checks))
	for i, check := range checks {
		results[i] = Result{Name: check.Name, Error: errNotCheckedYet.Error()}
	}
	return &Monitor{checks: checks, interval: interval, timeout: timeout, logger: logger, results: results}
}

// Run 马上检查一轮，之后每 interval 一轮，直到 ctx 结束。
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.checkAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkAll 并发跑一轮全部检查；状态变化时（正常 ↔ 出错）记一条日志，不每轮都记。
func (m *Monitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for i, check := range m.checks {
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(ctx, m.timeout)
			defer cancel()
			err := check.Probe(probeCtx)
			if ctx.Err() != nil { // 进程在退出：这一轮不算
				return
			}
			m.record(i, err)
		})
	}
	wg.Wait()
}

func (m *Monitor) record(i int, err error) {
	result := Result{Name: m.checks[i].Name, OK: err == nil, CheckedAt: time.Now()}
	if err != nil {
		result.Error = err.Error()
	}
	m.mu.Lock()
	previous := m.results[i]
	m.results[i] = result
	m.mu.Unlock()
	switch {
	case !result.OK && (previous.OK || previous.CheckedAt.IsZero()):
		m.logger.Warn("就绪检查：出错", "check", result.Name, "err", result.Error)
	case result.OK && !previous.OK && !previous.CheckedAt.IsZero():
		m.logger.Info("就绪检查：恢复正常", "check", result.Name)
	}
}

// Report 返回最近一次的结果：每一项都正常才算就绪。
func (m *Monitor) Report() Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	report := Report{Ready: true, Checks: append([]Result(nil), m.results...)}
	for _, result := range report.Checks {
		if !result.OK {
			report.Ready = false
		}
	}
	return report
}

// ServeHTTP 是 /readyz：就绪时 200，否则 503；内容是各项最近一次的结果（JSON）。
func (m *Monitor) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	report := m.Report()
	w.Header().Set("Content-Type", "application/json")
	if !report.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(report)
}

// Alive 是 /livez：进程还能响应就是活着（不看任何依赖：依赖出问题不该让 k8s 重启这个进程）。
func Alive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
