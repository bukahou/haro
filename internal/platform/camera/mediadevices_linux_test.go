//go:build linux

package camera

import (
	"testing"

	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"

	"github.com/bukahou/haro/internal/terminal"
)

func media(format frame.Format, width, height int, fps float32) prop.Media {
	return prop.Media{Video: prop.Video{Width: width, Height: height, FrameFormat: format, FrameRate: fps}}
}

func TestChooseCaptureMedia(t *testing.T) {
	want := terminal.FrameSize{Width: 1280, Height: 720}
	cases := []struct {
		name      string
		supported []prop.Media
		want      terminal.FrameSize
		expect    prop.Media
	}{
		{"正好有：同样大小优先 MJPEG（高分辨率下帧率高）",
			[]prop.Media{media(frame.FormatYUYV, 1280, 720, 10), media(frame.FormatMJPEG, 1280, 720, 30)}, want,
			media(frame.FormatMJPEG, 1280, 720, 30)},
		{"没有正好的：宽度够的里面挑最小",
			[]prop.Media{media(frame.FormatMJPEG, 640, 480, 30), media(frame.FormatMJPEG, 2560, 1440, 30),
				media(frame.FormatMJPEG, 1920, 1080, 30)}, want,
			media(frame.FormatMJPEG, 1920, 1080, 30)},
		{"都不够宽：挑最大",
			[]prop.Media{media(frame.FormatMJPEG, 320, 240, 30), media(frame.FormatMJPEG, 640, 480, 30)}, want,
			media(frame.FormatMJPEG, 640, 480, 30)},
		{"同样大小同样格式：挑帧率高的",
			[]prop.Media{media(frame.FormatMJPEG, 1280, 720, 15), media(frame.FormatMJPEG, 1280, 720, 30)}, want,
			media(frame.FormatMJPEG, 1280, 720, 30)},
		{"没说要多大：挑最大",
			[]prop.Media{media(frame.FormatMJPEG, 640, 480, 30), media(frame.FormatMJPEG, 1920, 1080, 30)}, terminal.FrameSize{},
			media(frame.FormatMJPEG, 1920, 1080, 30)},
	}
	for _, c := range cases {
		got, ok := chooseCaptureMedia(c.supported, c.want)
		if !ok || got.Video != c.expect.Video {
			t.Errorf("%s：得到 %+v", c.name, got.Video)
		}
	}
	if _, ok := chooseCaptureMedia([]prop.Media{media(frame.FormatMJPEG, 0, 0, 30)}, want); ok {
		t.Error("没有能用的分辨率时应返回 ok=false")
	}
}
