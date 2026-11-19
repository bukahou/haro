package camera

import (
	"image"
	"image/color"
	"testing"
)

func TestSettlerWaitsForThreeSteadyFrames(t *testing.T) {
	// 曝光爬升中（相差远超 0.5%），第 7、8、9 帧都和前一帧相差不到 0.5%
	lumas := []float64{20, 45, 80, 105, 118, 120, 120.2, 120.1, 120.3}
	var settler exposureSettler
	for i, luma := range lumas {
		if settler.observe(luma) {
			if i != 8 {
				t.Errorf("应在第 9 帧（连续 3 帧都稳）判稳，实际第 %d 帧", i+1)
			}
			return
		}
	}
	t.Fatal("一直没判稳")
}

func TestSettlerRestartsWhenBrightnessJumps(t *testing.T) {
	var settler exposureSettler
	for _, luma := range []float64{100, 100, 100, 130, 130, 130} {
		if settler.observe(luma) {
			t.Fatal("中间跳了一下，前面攒的稳定帧应清零，到最后一帧也只有 2 帧稳定")
		}
	}
	if !settler.observe(130) {
		t.Error("跳变后再连续 3 帧稳定，应判稳")
	}
}

func TestMeanLuma(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 64, 48))
	for i := range gray.Pix {
		gray.Pix[i] = 200
	}
	if got := meanLuma(gray); got < 199 || got > 201 {
		t.Errorf("灰度 200 的画面，平均亮度应约为 200，得到 %.1f", got)
	}
	ycbcr := image.NewYCbCr(image.Rect(0, 0, 64, 48), image.YCbCrSubsampleRatio420)
	for i := range ycbcr.Y {
		ycbcr.Y[i] = 50
	}
	if got := meanLuma(ycbcr); got != 50 {
		t.Errorf("YCbCr 画面直接读亮度平面，应为 50，得到 %.1f", got)
	}
}

func TestEncodeJPEGScalesDownOnly(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	img.Set(0, 0, color.White)
	if _, size, err := encodeJPEG(img, 640); err != nil || size != (image.Point{X: 640, Y: 360}) {
		t.Errorf("比上限宽：按比例缩到 640x360，得到 %v %v", size, err)
	}
	if _, size, err := encodeJPEG(img, 1920); err != nil || size != (image.Point{X: 1280, Y: 720}) {
		t.Errorf("比上限窄：原样，得到 %v %v", size, err)
	}
}
