package osdiff

import (
	"reflect"
	"testing"
)

// mode 是一个分辨率和帧率。
func mode(width, height int, fps float64) frameMode {
	return frameMode{size: frameSize{width, height}, fps: fps}
}

// c960 照 EMEET C960 报的格式：未压缩的只到 640×480，1280×720 只有 MJPEG；都是 30 帧。
var c960 = []v4l2Format{
	{name: "mjpeg", compressed: true, modes: []frameMode{mode(1920, 1080, 30), mode(1280, 960, 30), mode(1280, 720, 30),
		mode(1024, 576, 30), mode(800, 600, 30), mode(640, 480, 30), mode(640, 360, 30)}},
	{name: "yuyv422", modes: []frameMode{mode(640, 480, 30), mode(640, 360, 30)}},
}

func TestChooseV4L2Capture(t *testing.T) {
	both := []v4l2Format{
		{name: "mjpeg", compressed: true, modes: []frameMode{mode(1280, 720, 30), mode(640, 480, 30)}},
		{name: "yuyv422", modes: []frameMode{mode(1280, 720, 30), mode(640, 480, 30)}},
	}
	slowRaw := []v4l2Format{ // 未压缩的在高分辨率下只有几帧
		{name: "yuyv422", modes: []frameMode{mode(1280, 720, 10)}},
		{name: "mjpeg", compressed: true, modes: []frameMode{mode(1280, 720, 30)}},
	}
	big := []v4l2Format{{name: "mjpeg", compressed: true, modes: []frameMode{mode(3280, 2464, 15), mode(1920, 1080, 30), mode(1640, 1232, 30)}}}
	small := []v4l2Format{
		{name: "mjpeg", compressed: true, modes: []frameMode{mode(320, 240, 30)}},
		{name: "yuyv422", modes: []frameMode{mode(640, 480, 30), mode(320, 240, 30)}},
	}
	stepwise := []v4l2Format{{name: "yuv420p", ranges: []frameSizeRange{{32, 2592, 2, 32, 1944, 2}}}}
	cases := []struct {
		name               string
		formats            []v4l2Format
		wanted             string
		wantFormat, wantSz string
	}{
		{"C960：1280x720 只有 MJPEG 能出", c960, "1280x720", "mjpeg", "1280x720"},
		{"C960：640x480 两种都能出、帧率一样，要未压缩的", c960, "640x480", "yuyv422", "640x480"},
		{"同一分辨率两种格式帧率一样：要未压缩的", both, "1280x720", "yuyv422", "1280x720"},
		{"同一分辨率未压缩的帧率低：要帧率高的", slowRaw, "1280x720", "mjpeg", "1280x720"},
		{"没有想要的：宽度够的里面要最小的", big, "1280x720", "mjpeg", "1640x1232"},
		{"都不够宽：要最大的", small, "1280x720", "yuyv422", "640x480"},
		{"步进范围里有想要的", stepwise, "1280x720", "yuv420p", "1280x720"},
		{"在范围里但对不上步进：不算有", stepwise, "1281x721", "", "1281x721"},
		{"列不出格式：照旧由驱动定", nil, "1280x720", "", "1280x720"},
		{"分辨率写法认不出：照原样", c960, "hd720", "", "hd720"},
	}
	for _, c := range cases {
		if format, size := chooseV4L2Capture(c.formats, c.wanted); format != c.wantFormat || size != c.wantSz {
			t.Errorf("%s：得到 %s %s，应为 %s %s", c.name, format, size, c.wantFormat, c.wantSz)
		}
	}
}

func TestV4L2Capture(t *testing.T) {
	got := v4l2Capture("/dev/video0", c960, "1280x720")
	want := CameraCapture{Input: []string{"-f", "v4l2", "-input_format", "mjpeg", "-video_size", "1280x720", "-i", "/dev/video0"},
		Format: "mjpeg", Size: "1280x720"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("C960 的输入参数不对：%+v", got)
	}
	if got := v4l2Capture("/dev/video0", nil, "1280x720"); !reflect.DeepEqual(got.Input,
		[]string{"-f", "v4l2", "-video_size", "1280x720", "-i", "/dev/video0"}) || got.Format != "" {
		t.Errorf("列不出格式时应不指定格式：%+v", got)
	}
}

// 像素格式的四字符码按内核的写法算（videodev2.h：V4L2_PIX_FMT_MJPEG = 0x47504a4d、YUYV = 0x56595559）。
func TestFFmpegPixelFormats(t *testing.T) {
	if v4l2FourCC("MJPG") != 0x47504a4d || v4l2FourCC("YUYV") != 0x56595559 {
		t.Fatalf("四字符码算错了：%#x %#x", v4l2FourCC("MJPG"), v4l2FourCC("YUYV"))
	}
	if f := ffmpegPixelFormats[0x47504a4d]; f.name != "mjpeg" || !f.compressed {
		t.Errorf("MJPEG：%+v", f)
	}
	if f := ffmpegPixelFormats[0x56595559]; f.name != "yuyv422" || f.compressed {
		t.Errorf("YUYV：%+v", f)
	}
}
