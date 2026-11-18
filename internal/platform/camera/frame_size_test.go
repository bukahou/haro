package camera

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFrameSizeLogOnlyOnChange(t *testing.T) {
	var buf syncBuffer
	sizes := newFrameSizeLog(testLogger(&buf))
	for _, frame := range [][]byte{testJPEG(t, 1280, 720), testJPEG(t, 1280, 720), testJPEG(t, 640, 480), []byte("不是 JPEG")} {
		sizes.observe(frame)
	}
	if buf.count("送给模型的画面") != 2 || buf.count("size=1280x720") != 1 || buf.count("size=640x480") != 1 {
		t.Errorf("应只在第一帧和大小变了时各记一条：\n%s", buf.buf.String())
	}
}

func TestScaleFilterNeverEnlarges(t *testing.T) {
	if got := scaleFilter(1280); got != "scale='min(1280,iw)':-2" {
		t.Errorf("缩放滤镜不对：%s", got)
	}
}
