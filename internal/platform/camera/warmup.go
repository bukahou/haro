package camera

import (
	"image"
	"math"
	"time"
)

// 曝光预热：摄像头刚打开时自动曝光还没调好，头几帧偏暗或偏亮。一直读帧，直到亮度稳定再取那一帧。
// 规则：连续 stableFrames 帧，每帧和前一帧的平均亮度相差都不到 stableChange；最多等 warmupLimit，到时就取最新一帧。
// 室内实测约第 7 帧（打开后约 0.25 秒）稳定；暗处可能要等满上限。
const (
	stableFrames = 3
	stableChange = 0.005
	warmupLimit  = 1200 * time.Millisecond
)

// exposureSettler 逐帧判断画面亮度稳定了没有。
type exposureSettler struct {
	frames   int
	previous float64
	steady   int // 连续几帧和前一帧相差不到 stableChange
}

// observe 记下新一帧的平均亮度（0–255），返回现在是否已经稳定。
func (s *exposureSettler) observe(luma float64) bool {
	s.frames++
	if s.frames > 1 && math.Abs(luma-s.previous) < stableChange*math.Max(s.previous, 1) {
		s.steady++
	} else {
		s.steady = 0
	}
	s.previous = luma
	return s.steady >= stableFrames
}

// lumaStep 是算平均亮度时的取样间隔（像素）：只为判断稳定，不必逐个像素算。
const lumaStep = 8

// meanLuma 按网格取样算平均亮度（0–255）；YCbCr 的画面直接读亮度平面。
func meanLuma(img image.Image) float64 {
	bounds := img.Bounds()
	var sum float64
	var count int
	if ycbcr, ok := img.(*image.YCbCr); ok {
		for y := bounds.Min.Y; y < bounds.Max.Y; y += lumaStep {
			for x := bounds.Min.X; x < bounds.Max.X; x += lumaStep {
				sum += float64(ycbcr.Y[ycbcr.YOffset(x, y)])
				count++
			}
		}
	} else {
		for y := bounds.Min.Y; y < bounds.Max.Y; y += lumaStep {
			for x := bounds.Min.X; x < bounds.Max.X; x += lumaStep {
				r, g, b, _ := img.At(x, y).RGBA()
				sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
				count++
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}
