package camera

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"log/slog"
	"sync"
)

// scaleFilter 是 ffmpeg 的缩放滤镜：画面比 maxWidth 宽就缩到 maxWidth，否则原样（放大不会多出细节，只多花 token）；
// 高度按比例、取偶数。
func scaleFilter(maxWidth int) string {
	return fmt.Sprintf("scale='min(%d,iw)':-2", maxWidth)
}

// frameSizeLog 记下送给模型的画面实际多大：第一帧和大小变了时各写一条。
// 送出去多大最终取决于摄像头给了多大，只看配置看不出来。
type frameSizeLog struct {
	logger *slog.Logger

	mu   sync.Mutex
	last string
}

func newFrameSizeLog(logger *slog.Logger) *frameSizeLog { return &frameSizeLog{logger: logger} }

// observe 读 JPEG 头里的宽高（不解码整张图）。
func (f *frameSizeLog) observe(frame []byte) {
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return
	}
	size := fmt.Sprintf("%dx%d", config.Width, config.Height)
	f.mu.Lock()
	defer f.mu.Unlock()
	if size != f.last {
		f.last = size
		f.logger.Info("📷 送给模型的画面", "size", size, "bytes", len(frame))
	}
}
