package camera

import (
	"context"
	"log/slog"
	"sync"
)

// Locate 找现在要用的摄像头：返回 ffmpeg 的输入参数与名字；ok=false 表示现在没有摄像头。
// 由平台层用服务层的选择规则组装（本包不认系统、不认规则）。取帧失败后会重新调用它，
// 所以摄像头被拔掉再插上、换了设备号（/dev/video0 → /dev/video2），都能自己接上。
type Locate func(ctx context.Context) (input []string, name string, ok bool)

// health 记摄像头现在能不能用，只在状态变化时写日志（不可用时不再每次重试都刷一条）。
type health struct {
	logger *slog.Logger

	mu      sync.Mutex
	name    string // 现在用的摄像头名字
	up      bool   // 能用
	wasDown bool   // 曾经不可用过（恢复时才说「恢复了」）
}

func newHealth(name string, logger *slog.Logger) *health {
	return &health{logger: logger, name: name, up: true}
}

// markUp：拿到画面了。
func (h *health) markUp(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.up && h.wasDown {
		h.logger.Info("📷 摄像头恢复了", "device", name)
	}
	h.name, h.up = name, true
}

// markDown：取不到画面。第一次写 WARN，之后只写 DEBUG。
func (h *health) markDown(reason string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.up {
		h.logger.Warn("📷 摄像头不可用：之后自动重试，恢复时再记一条", "device", h.name, "why", reason, "err", err)
	} else {
		h.logger.Debug("摄像头仍不可用", "why", reason, "err", err)
	}
	h.up, h.wasDown = false, true
}

// currentName 是现在能用的摄像头名字；不可用时为空。
func (h *health) currentName() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.up {
		return ""
	}
	return h.name
}
