package camera

import (
	"bytes"
	"image"
	"image/jpeg"

	"golang.org/x/image/draw"

	"github.com/bukahou/haro/internal/terminal"
)

// encodeJPEG 把画面缩到不超过 maxWidth（更窄就原样：放大不会多出细节，只多花 token），编码成 JPEG。
func encodeJPEG(img image.Image, maxWidth int) ([]byte, image.Point, error) {
	bounds := img.Bounds()
	if maxWidth > 0 && bounds.Dx() > maxWidth {
		height := max(bounds.Dy()*maxWidth/bounds.Dx(), 1)
		scaled := image.NewRGBA(image.Rect(0, 0, maxWidth, height))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, bounds, draw.Src, nil)
		img = scaled
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: terminal.FrameJPEGQuality}); err != nil {
		return nil, image.Point{}, err
	}
	return encoded.Bytes(), img.Bounds().Size(), nil
}
